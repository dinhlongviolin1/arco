package telegram

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/dinhlongviolin1/arco/internal/core"
)

const (
	longPollSec        = 25 // server-side long-poll block
	pollHTTPGrace      = 10 * time.Second
	inboundBackoff     = 3 * time.Second
	statusTickInterval = 45 * time.Second
)

// Start runs the inbound long-poll loop until ctx is cancelled, and on a ticker
// refreshes open sessions' status cards so they stay live even between cards.
// Blocking; the daemon runs it in a goroutine joined to its shutdown ctx.
func (b *Bot) Start(ctx context.Context) {
	offset := 0
	ticker := time.NewTicker(statusTickInterval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ticker.C:
			b.refreshAllStatus(ctx)
		default:
		}
		pollCtx, cancel := context.WithTimeout(ctx, time.Duration(longPollSec)*time.Second+pollHTTPGrace)
		ups, err := b.api.GetUpdates(pollCtx, offset, longPollSec)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(inboundBackoff): // transient (network/rate) — back off, keep polling
			}
			continue
		}
		for _, u := range ups {
			offset = int(u.UpdateID) + 1
			b.handleUpdate(ctx, u)
		}
	}
}

// handleUpdate routes one update. Authorization is enforced per-branch (a
// stranger's message and a stranger's button tap are both dropped).
func (b *Bot) handleUpdate(ctx context.Context, u Update) {
	switch {
	case u.CallbackQuery != nil:
		b.handleCallback(ctx, u.CallbackQuery)
	case u.Message != nil && (len(u.Message.Photo) > 0 || u.Message.Document != nil):
		b.handleInboundImage(ctx, u.Message)
	case u.Message != nil:
		b.handleMessage(ctx, u.Message)
	}
}

// handleCallback turns an authorized button tap into an engine action and
// acknowledges the query with a toast. An unauthorized or malformed tap is
// answered with a refusal and does nothing else.
func (b *Bot) handleCallback(ctx context.Context, cq *CallbackQuery) {
	if !b.allowed[cq.From.ID] {
		_ = b.api.AnswerCallbackQuery(ctx, cq.ID, "not authorized")
		return
	}
	act, escID, err := DecodeCallback(cq.Data)
	if err != nil {
		_ = b.api.AnswerCallbackQuery(ctx, cq.ID, "unknown action")
		return
	}
	toast := b.dispatchAction(ctx, act, escID)
	_ = b.api.AnswerCallbackQuery(ctx, cq.ID, toast)
}

// dispatchAction executes one decoded button action and returns the toast text.
func (b *Bot) dispatchAction(ctx context.Context, act Action, escID string) string {
	esc, err := b.store.GetEscalation(escID)
	if err != nil {
		return "escalation not found"
	}
	switch act {
	case ActQuestionOnce:
		return b.answerQuestion(ctx, esc, core.ScopeOnce)
	case ActQuestionSession:
		return b.answerQuestion(ctx, esc, core.ScopeSession)
	case ActQuestionNo:
		if err := b.actions.AnswerQuestion(ctx, esc.ID, "No.", core.ScopeOnce); err != nil {
			return "failed: " + err.Error()
		}
		return "❌ answered: no"
	case ActConfirmYes:
		if err := b.actions.DecideConfirm(ctx, esc.ID, true, core.ScopeOnce); err != nil {
			return "failed: " + err.Error()
		}
		return "✅ approved"
	case ActConfirmNo:
		if err := b.actions.DecideConfirm(ctx, esc.ID, false, core.ScopeOnce); err != nil {
			return "failed: " + err.Error()
		}
		return "❌ rejected"
	case ActDiff:
		b.sendDiff(ctx, esc)
		return "diff posted"
	}
	return "unknown action"
}

// answerQuestion accepts the brain's DRAFT answer (the operator's tap is the
// human decision); an empty draft falls back to a plain proceed.
func (b *Bot) answerQuestion(ctx context.Context, esc core.Escalation, scope core.Scope) string {
	text := esc.DraftAnswer
	if strings.TrimSpace(text) == "" {
		text = "Yes — proceed."
	}
	if err := b.actions.AnswerQuestion(ctx, esc.ID, text, scope); err != nil {
		return "failed: " + err.Error()
	}
	if scope == core.ScopeSession {
		return "✅ answered (standing for session)"
	}
	return "✅ answered"
}

// sendDiff posts a worker's redacted diff into its session topic.
func (b *Bot) sendDiff(ctx context.Context, esc core.Escalation) {
	if esc.WorkerID == "" {
		return
	}
	var thread int64
	if esc.SessionID != "" {
		thread, _ = b.ensureTopic(ctx, esc.SessionID)
	}
	patch, err := b.actions.WorkerDiff(ctx, esc.WorkerID)
	if err != nil {
		_, _ = b.api.SendMessage(ctx, SendMessageReq{ChatID: b.groupID, MessageThreadID: thread, Text: "diff error: " + err.Error()})
		return
	}
	if b.redact != nil {
		patch, _ = b.redact.Scrub(patch)
	}
	if strings.TrimSpace(patch) == "" {
		patch = "(no diff — base == head)"
	}
	_, _ = b.api.SendMessage(ctx, SendMessageReq{
		ChatID:          b.groupID,
		MessageThreadID: thread,
		Text:            truncate("diff — "+esc.WorkerID+"\n\n"+patch, tgMessageCap),
	})
}

// handleMessage handles an authorized slash-command in the General topic.
func (b *Bot) handleMessage(ctx context.Context, m *Message) {
	if m.From == nil || !b.allowed[m.From.ID] {
		return
	}
	text := strings.TrimSpace(m.Text)
	if !strings.HasPrefix(text, "/") {
		return
	}
	cmd := strings.Fields(text)[0]
	cmd = strings.SplitN(cmd, "@", 2)[0] // strip /cmd@botname
	var reply string
	switch cmd {
	case "/status":
		reply = b.fleetStatus()
	case "/pause":
		if err := b.actions.Pause(ctx); err != nil {
			reply = "pause failed: " + err.Error()
		} else {
			reply = "⛔ estop engaged — no new workers, no autonomous actions (in-flight work keeps running)"
		}
	case "/resume":
		if err := b.actions.Resume(ctx); err != nil {
			reply = "resume failed: " + err.Error()
		} else {
			reply = "▶️ estop released"
		}
	case "/help":
		reply = "arco console — commands:\n/status — fleet summary\n/pause — engage emergency stop\n/resume — release it"
	default:
		return // ignore unknown commands silently
	}
	_, _ = b.api.SendMessage(ctx, SendMessageReq{ChatID: b.groupID, MessageThreadID: m.MessageThreadID, Text: reply})
}

// fleetStatus renders the /status reply: estop state + a by-state tally of
// non-terminal workers + the pending-decision count.
func (b *Bot) fleetStatus() string {
	workers, _ := b.store.ListWorkers(core.WorkerFilter{})
	pending, _ := b.store.ListEscalations(core.EscalationFilter{Status: "pending"})
	counts := map[string]int{}
	active := 0
	for _, w := range workers {
		if w.State.Terminal() {
			continue
		}
		counts[string(w.State)]++
		active++
	}
	var b2 strings.Builder
	if b.actions.Paused() {
		b2.WriteString("⛔ ESTOP ENGAGED\n")
	} else {
		b2.WriteString("▶️ running\n")
	}
	fmt.Fprintf(&b2, "active workers: %d\n", active)
	if active > 0 {
		b2.WriteString(tally(counts))
		b2.WriteByte('\n')
	}
	fmt.Fprintf(&b2, "⏳ pending decisions: %d", len(pending))
	return b2.String()
}

// refreshAllStatus re-renders the status card of every non-terminal, non-pool
// session that already has a topic (the ticker's liveness sweep).
func (b *Bot) refreshAllStatus(ctx context.Context) {
	sessions, err := b.store.ListSessions(core.SessionFilter{})
	if err != nil {
		return
	}
	for _, s := range sessions {
		if s.Kind == core.SessionKindPool {
			continue
		}
		if s.TGTopicID == nil || *s.TGTopicID == 0 {
			continue
		}
		if s.Status == core.SessionDone || s.Status == core.SessionArchived {
			b.maybeCloseTopic(ctx, s.ID, *s.TGTopicID)
			continue
		}
		b.refreshStatus(ctx, s.ID, *s.TGTopicID)
	}
}

// maybeCloseTopic closes a finished session's forum topic ONCE (history is
// preserved — close, never delete). Tracked in-memory so the ticker doesn't
// re-close every sweep.
func (b *Bot) maybeCloseTopic(ctx context.Context, sessionID string, threadID int64) {
	b.mu.Lock()
	if b.closed[sessionID] {
		b.mu.Unlock()
		return
	}
	b.closed[sessionID] = true
	b.mu.Unlock()
	// Post a final status line, then close.
	if s, err := b.store.GetSession(sessionID); err == nil {
		workers, _ := b.store.ListWorkers(core.WorkerFilter{OwnerSession: sessionID})
		_, _ = b.api.SendMessage(ctx, SendMessageReq{
			ChatID: b.groupID, MessageThreadID: threadID,
			Text: "🏁 session " + string(s.Status) + "\n" + renderStatus(s, workers, 0),
		})
	}
	_ = b.api.CloseForumTopic(ctx, b.groupID, threadID)
}

func tally(counts map[string]int) string {
	states := make([]string, 0, len(counts))
	for st := range counts {
		states = append(states, st)
	}
	sort.Strings(states)
	parts := make([]string, 0, len(states))
	for _, st := range states {
		parts = append(parts, fmt.Sprintf("%s×%d", st, counts[st]))
	}
	return strings.Join(parts, "  ")
}
