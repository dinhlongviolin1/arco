package telegram

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/notify"
)

// api is the Bot API surface the Bot depends on (Client satisfies it). An
// interface, not the concrete *Client, so the Bot is unit-testable against a
// fake without a network.
type api interface {
	SendMessage(ctx context.Context, req SendMessageReq) (Message, error)
	EditMessageText(ctx context.Context, req EditMessageTextReq) error
	CreateForumTopic(ctx context.Context, chatID int64, name string) (int64, error)
	CloseForumTopic(ctx context.Context, chatID, threadID int64) error
	PinChatMessage(ctx context.Context, chatID, messageID int64) error
	AnswerCallbackQuery(ctx context.Context, id, text string) error
	GetUpdates(ctx context.Context, offset, timeoutSec int) ([]Update, error)
	// Image relay (T6).
	SendPhoto(ctx context.Context, req SendPhotoReq) (Message, error)
	GetFile(ctx context.Context, fileID string) (File, error)
	DownloadFile(ctx context.Context, filePath string) ([]byte, error)
	// SetMyCommands registers the client-side command menu.
	SetMyCommands(ctx context.Context, cmds []BotCommand) error
}

// Actions is the engine surface the bot drives from an inbound button tap or a
// General-topic command. reconcile.Engine provides AnswerQuestion/DecideConfirm
// directly; the daemon wraps the rest (diff formatting, estop) in an adapter, so
// this package never imports reconcile.
type Actions interface {
	AnswerQuestion(ctx context.Context, escID, text string, scope core.Scope) error
	DecideConfirm(ctx context.Context, escID string, yes bool, scope core.Scope) error
	// WorkerDiff returns a worker's unified diff patch (RAW — the bot redacts it
	// before it leaves for Telegram).
	WorkerDiff(ctx context.Context, workerID string) (string, error)
	// Pause/Resume engage/release the operator emergency stop (General console).
	Pause(ctx context.Context) error
	Resume(ctx context.Context) error
	// Paused reports whether the emergency stop is currently engaged (for /status).
	Paused() bool
	// Dispatch spawns a worker on repo to do task (the /dispatch command), on the
	// given VM ("" = engine default). into is the session/issue to add the agent
	// to ("" = start a new issue), returning the new worker + session ids.
	Dispatch(ctx context.Context, repo, task, vm, into string) (workerID, sessionID string, err error)
	// Kill terminates a worker (the /kill command).
	Kill(ctx context.Context, workerID string) error
	// BrainReply is a conversational reply from arco's brain (free-text chat).
	BrainReply(ctx context.Context, prompt string) (string, error)
}

// Store is the narrow ledger surface the bot reads/writes — just the session/
// worker/escalation reads plus the topic-id binding. core.Store satisfies it via
// the daemon's small adapter; tests fake it directly (no need to implement the
// whole core.Reader/core.Tx).
type Store interface {
	GetSession(id string) (core.Session, error)
	GetWorker(id string) (core.Worker, error)
	GetEscalation(id string) (core.Escalation, error)
	ListWorkers(f core.WorkerFilter) ([]core.Worker, error)
	ListSessions(f core.SessionFilter) ([]core.Session, error)
	ListEscalations(f core.EscalationFilter) ([]core.Escalation, error)
	SetSessionTelegram(ctx context.Context, sessionID string, topicID, statusMsgID *int64) error
}

// Config builds a Bot.
type Config struct {
	API      api
	Store    Store
	GroupID  int64
	MinLevel notify.Level
	Actions  Actions
	Allowed  []int64
	Redact   core.Scrubber // scrubs diffs before they leave for Telegram (nil = no scrub)
	// VMs are display lines for the attached fleet (e.g. "local (default · this
	// box)", "vm1 (host vm1)"), for the /vms command + chat context. The daemon
	// builds them from config so factual fleet questions aren't brain-guessed.
	VMs []string
}

// Bot is the Telegram forum notifier + inbound driver. It implements
// notify.Sender (outbound cards, topic-routed) and Start (inbound long-poll).
type Bot struct {
	api     api
	store   Store
	groupID int64
	min     notify.Level
	actions Actions
	allowed map[int64]bool
	redact  core.Scrubber
	vms     []string

	mu       sync.Mutex
	locks    map[string]*sync.Mutex // per-session lock serializing topic create/status edit
	lastEdit map[string]time.Time   // per-session status-card edit throttle
	closed   map[string]bool        // sessions whose topic we've already closed (idempotence)
	escMsg   map[string]int64       // escalation id → its card message id (to edit on resolve)
	msgEsc   map[int64]string       // reverse: card message id → escalation id (swipe-reply routing)
}

// New builds a Bot from cfg.
func New(cfg Config) *Bot {
	allowed := make(map[int64]bool, len(cfg.Allowed))
	for _, id := range cfg.Allowed {
		allowed[id] = true
	}
	return &Bot{
		api:      cfg.API,
		store:    cfg.Store,
		groupID:  cfg.GroupID,
		min:      cfg.MinLevel,
		actions:  cfg.Actions,
		allowed:  allowed,
		redact:   cfg.Redact,
		vms:      cfg.VMs,
		locks:    map[string]*sync.Mutex{},
		lastEdit: map[string]time.Time{},
		closed:   map[string]bool{},
		escMsg:   map[string]int64{},
		msgEsc:   map[int64]string{},
	}
}

const (
	// tgMessageCap is Telegram's per-message text limit (4096); we stay well under.
	tgMessageCap = 3800
	// statusEditMinInterval throttles per-session status-card edits so a burst of
	// cards can't trip Telegram's ~1 edit/sec/chat rate limit.
	statusEditMinInterval = 2 * time.Second
)

// Send implements notify.Sender: it routes a card to its session's forum topic
// (creating the topic on first use), refreshes that topic's pinned status card,
// and — for an open escalation — attaches the answer keyboard. Below-min and
// per-session-muted cards are dropped. Best-effort: any error is returned to the
// engine's notifyCard, which logs and swallows it.
func (b *Bot) Send(ctx context.Context, c notify.Card) error {
	// A resolution card edits the original escalation message in place (stripping
	// its now-stale buttons) — handled BEFORE the min filter so an answered card
	// always cleans up, even under a high min_level, and without spamming a new
	// message. Falls through to a normal send only if we don't know the card id.
	if c.Resolved && c.EscalationID != "" {
		if b.editResolved(ctx, c) {
			return nil
		}
	}
	if c.Level < b.min {
		return nil
	}
	var threadID int64
	if c.SessionID != "" {
		if muted, err := b.sessionMutes(c); err == nil && muted {
			return nil
		}
		tid, err := b.ensureTopic(ctx, c.SessionID)
		if err != nil {
			return err
		}
		threadID = tid
		b.refreshStatus(ctx, c.SessionID, threadID)
	}

	open := c.EscalationID != "" && !c.Resolved
	req := SendMessageReq{ChatID: b.groupID, MessageThreadID: threadID, Text: cardText(c)}
	if open {
		kb := keyboardFor(c.EscalationKind, c.EscalationID)
		req.ReplyMarkup = &kb
	}
	m, err := b.api.SendMessage(ctx, req)
	if err != nil {
		return err
	}
	if open {
		// Remember the card's message id so the later resolution can strip its
		// buttons, and mirror to General so a muted/unwatched topic can't hide a
		// decision the operator must make.
		b.mu.Lock()
		b.escMsg[c.EscalationID] = m.MessageID
		b.msgEsc[m.MessageID] = c.EscalationID
		b.mu.Unlock()
		if threadID != 0 {
			_, _ = b.api.SendMessage(ctx, SendMessageReq{
				ChatID: b.groupID,
				Text:   "🔔 decision needed in topic — " + firstLine(c.Title),
			})
		}
	}
	return nil
}

// editResolved edits a resolved escalation's original card in place: it replaces
// the text with the resolution and removes the inline keyboard (an empty markup
// clears it), so the buttons can't be tapped again. Returns false if we don't
// know the card's message id (e.g. daemon restarted after the card was posted),
// letting Send fall back to posting a normal message.
func (b *Bot) editResolved(ctx context.Context, c notify.Card) bool {
	b.mu.Lock()
	mid := b.escMsg[c.EscalationID]
	b.mu.Unlock()
	if mid == 0 {
		return false
	}
	empty := InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{}}
	err := b.api.EditMessageText(ctx, EditMessageTextReq{
		ChatID: b.groupID, MessageID: mid, Text: cardText(c), ReplyMarkup: &empty,
	})
	if err != nil && !IsNotModified(err) {
		return false
	}
	b.mu.Lock()
	delete(b.escMsg, c.EscalationID)
	delete(b.msgEsc, mid)
	b.mu.Unlock()
	return true
}

// escForReplyTo returns the escalation id a swipe-reply targets (the card the
// operator replied to), if it's a known open escalation card.
func (b *Bot) escForReplyTo(m *Message) (string, bool) {
	if m.ReplyToMessage == nil {
		return "", false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	esc, ok := b.msgEsc[m.ReplyToMessage.MessageID]
	return esc, ok
}

// sessionMutes reports whether a session's notify_level suppresses this card.
// "silent" mutes everything; "important" mutes info-level; "all"/"" mutes
// nothing. (Global severity is the b.min floor; this is the per-session dial.)
func (b *Bot) sessionMutes(c notify.Card) (bool, error) {
	s, err := b.store.GetSession(c.SessionID)
	if err != nil {
		return false, err
	}
	switch s.NotifyLevel {
	case "silent":
		return true, nil
	case "important":
		return c.Level < notify.LevelWarn, nil
	default: // "all", "", anything else
		return false, nil
	}
}

// ensureTopic returns a session's forum topic id, creating (and persisting) it
// on first use. Serialized per session so two concurrent cards can't create two
// topics for the same session.
func (b *Bot) ensureTopic(ctx context.Context, sessionID string) (int64, error) {
	lock := b.sessionLock(sessionID)
	lock.Lock()
	defer lock.Unlock()

	s, err := b.store.GetSession(sessionID)
	if err != nil {
		return 0, err
	}
	if s.TGTopicID != nil && *s.TGTopicID != 0 {
		return *s.TGTopicID, nil
	}
	name := topicName(s)
	// A dispatch-created session often has no slug/title/goal → topicName would
	// degrade to "session: session <id>". Fall back to a worker's task so the
	// topic is human-readable ("working with topics").
	if s.Slug == "" && s.Title == "" && s.Goal == "" {
		if ws, _ := b.store.ListWorkers(core.WorkerFilter{OwnerSession: sessionID}); len(ws) > 0 {
			for _, w := range ws {
				if w.Task != "" {
					name = "session: " + truncate(w.Task, 60)
					break
				}
			}
		}
	}
	tid, err := b.api.CreateForumTopic(ctx, b.groupID, name)
	if err != nil {
		return 0, err
	}
	// Persist immediately (before the status card) so a status-card failure never
	// causes the topic to be recreated on the next card.
	if err := b.store.SetSessionTelegram(ctx, sessionID, &tid, nil); err != nil {
		return 0, err
	}
	return tid, nil
}

// refreshStatus upserts a session's pinned status card: sends+pins it on first
// use, else edits it in place (no new notification). Throttled per session and
// fully best-effort — a status-card failure never blocks the actual card.
func (b *Bot) refreshStatus(ctx context.Context, sessionID string, threadID int64) {
	b.mu.Lock()
	if last, ok := b.lastEdit[sessionID]; ok && b.now().Sub(last) < statusEditMinInterval {
		b.mu.Unlock()
		return
	}
	b.lastEdit[sessionID] = b.now()
	b.mu.Unlock()

	s, err := b.store.GetSession(sessionID)
	if err != nil {
		return
	}
	workers, _ := b.store.ListWorkers(core.WorkerFilter{OwnerSession: sessionID})
	pending, _ := b.store.ListEscalations(core.EscalationFilter{SessionID: sessionID, Status: "pending"})
	text := renderStatus(s, workers, len(pending))

	if s.TGStatusMsgID == nil || *s.TGStatusMsgID == 0 {
		m, err := b.api.SendMessage(ctx, SendMessageReq{ChatID: b.groupID, MessageThreadID: threadID, Text: text})
		if err != nil {
			return
		}
		_ = b.api.PinChatMessage(ctx, b.groupID, m.MessageID)
		msgID := m.MessageID
		_ = b.store.SetSessionTelegram(ctx, sessionID, nil, &msgID)
		return
	}
	err = b.api.EditMessageText(ctx, EditMessageTextReq{ChatID: b.groupID, MessageID: *s.TGStatusMsgID, Text: text})
	if err != nil && !IsNotModified(err) {
		return // benign; next refresh retries
	}
}

func (b *Bot) sessionLock(sessionID string) *sync.Mutex {
	b.mu.Lock()
	defer b.mu.Unlock()
	m := b.locks[sessionID]
	if m == nil {
		m = &sync.Mutex{}
		b.locks[sessionID] = m
	}
	return m
}

func (b *Bot) now() time.Time { return time.Now() }

// keyboardFor picks the answer keyboard for an escalation kind.
func keyboardFor(kind, escID string) InlineKeyboardMarkup {
	if kind == "confirm" {
		return ConfirmKeyboard(escID)
	}
	return QuestionKeyboard(escID)
}

// cardText renders a card's title+body into a single Telegram message, capped.
func cardText(c notify.Card) string {
	text := c.Title
	if c.Body != "" {
		text += "\n" + c.Body
	}
	return truncate(text, tgMessageCap)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// rune-align the cut so a multi-byte rune is never split.
	cut := n
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut] + "\n… (truncated)"
}

// utf8Start reports whether b is a UTF-8 leading byte (not a continuation byte).
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

var _ notify.Sender = (*Bot)(nil)

// compile-time: Client satisfies api.
var _ api = (*Client)(nil)
