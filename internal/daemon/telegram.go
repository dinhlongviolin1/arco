package daemon

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/dinhlongviolin1/arco/internal/config"
	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/feature"
	"github.com/dinhlongviolin1/arco/internal/features"
	"github.com/dinhlongviolin1/arco/internal/notify"
	"github.com/dinhlongviolin1/arco/internal/reconcile"
	"github.com/dinhlongviolin1/arco/internal/telegram"
)

// engineActions adapts *reconcile.Engine to telegram.Actions: it maps a button
// tap / console command onto the same engine entrypoints the CLI uses, so the
// telegram package never imports reconcile.
type engineActions struct {
	eng       *reconcile.Engine
	estopPath string
}

func (a engineActions) AnswerQuestion(ctx context.Context, escID, text string, scope core.Scope) error {
	return a.eng.AnswerQuestion(ctx, escID, text, scope)
}

func (a engineActions) DecideConfirm(ctx context.Context, escID string, yes bool, scope core.Scope) error {
	return a.eng.DecideConfirm(ctx, escID, yes, scope)
}

func (a engineActions) WorkerDiff(ctx context.Context, workerID string) (string, error) {
	d, err := a.eng.WorkerDiff(ctx, workerID)
	if err != nil {
		return "", err
	}
	patch := d.Patch
	if d.Truncated {
		patch += "\n… (diff truncated by arco)"
	}
	return patch, nil
}

// Pause/Resume write/remove the estop sentinel directly (same mechanism as the
// `arco pause`/`resume` CLI — the estop must work without the socket).
func (a engineActions) Pause(context.Context) error {
	return os.WriteFile(a.estopPath, []byte("engaged via telegram\n"), 0o600)
}

func (a engineActions) Resume(context.Context) error {
	if err := os.Remove(a.estopPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (a engineActions) Paused() bool { return a.eng.Paused() }

func (a engineActions) Dispatch(ctx context.Context, repo, task, vm, into string) (string, string, error) {
	// The repo-based spawn path (a real worker on a fresh per-worker worktree),
	// the same one `arco dispatch --repo` uses; vm picks the target ("" = default).
	// into = an existing session/issue to add the aspect to ("" = a new issue).
	res, err := a.eng.Spawn(ctx, into, task, into == "", repo, "", vm)
	if err != nil {
		return "", "", err
	}
	return res.WorkerID, res.SessionID, nil
}

func (a engineActions) BrainReply(ctx context.Context, prompt string) (string, error) {
	return a.eng.BrainReply(ctx, prompt)
}

func (a engineActions) Converse(ctx context.Context, system, prompt, sessionID string, tools []feature.Tool) (string, error) {
	return a.eng.Converse(ctx, system, prompt, sessionID, tools)
}

// Scan lists live herdr agents across the fleet. Engine and telegram now share
// core.ScannedAgent, so this is a straight passthrough — no adapter conversion.
func (a engineActions) Scan(ctx context.Context) ([]core.ScannedAgent, error) {
	return a.eng.ScanAgents(ctx)
}

// contextStore adapts core.Store to telegram.ContextStore — durable, per-session
// chat history. Appends run in a one-statement write tx (content scrubbed at the
// ledger chokepoint); reads take the newest `limit` messages via the Reader.
type contextStore struct{ s core.Store }

func (c contextStore) AppendMessage(ctx context.Context, sessionID, role, content string) error {
	return c.s.WithTx(ctx, func(tx core.Tx) error {
		_, err := tx.AppendMessage(core.Message{SessionID: sessionID, Role: role, Content: content})
		return err
	})
}

// chatHistoryWindow bounds durable chat history to the recent past, so a channel
// idle for days doesn't resurface stale context on the next message (the newest
// `limit` still caps it). Uses the store's injected clock (test-controllable).
const chatHistoryWindow = 72 * time.Hour

func (c contextStore) RecentMessages(sessionID string, limit int) ([]telegram.ContextMessage, error) {
	since := c.s.Now().Add(-chatHistoryWindow)
	msgs, err := c.s.Reader().RecentMessages(sessionID, since, limit)
	if err != nil {
		return nil, err
	}
	out := make([]telegram.ContextMessage, len(msgs))
	for i, m := range msgs {
		out[i] = telegram.ContextMessage{Role: m.Role, Content: m.Content}
	}
	return out, nil
}

// ensureConsoleSession idempotently creates the console sentinel session so
// General-topic chat (which has no work session) can append durable history
// despite brain_transcript_rows' FK to sessions. A plain `work` session at a fixed
// well-known id — created once, skipped thereafter.
func ensureConsoleSession(ctx context.Context, store core.Store) error {
	if s, err := store.Reader().GetSession(core.ConsoleSessionID); err == nil && s.ID != "" {
		return nil
	}
	return store.WithTx(ctx, func(tx core.Tx) error {
		return tx.CreateSession(core.Session{
			ID:     core.ConsoleSessionID,
			Kind:   core.SessionKindWork,
			Status: core.SessionActive,
			Slug:   "console",
			Title:  "Console (General chat)",
		})
	})
}

// tgStore adapts core.Store to telegram.Store: reads go through Reader(); the
// topic-id binding runs in a one-statement write tx.
type tgStore struct{ s core.Store }

func (t tgStore) GetSession(id string) (core.Session, error) { return t.s.Reader().GetSession(id) }
func (t tgStore) GetWorker(id string) (core.Worker, error)   { return t.s.Reader().GetWorker(id) }
func (t tgStore) GetEscalation(id string) (core.Escalation, error) {
	return t.s.Reader().GetEscalation(id)
}
func (t tgStore) ListWorkers(f core.WorkerFilter) ([]core.Worker, error) {
	return t.s.Reader().ListWorkers(f)
}
func (t tgStore) ListSessions(f core.SessionFilter) ([]core.Session, error) {
	return t.s.Reader().ListSessions(f)
}
func (t tgStore) ListEscalations(f core.EscalationFilter) ([]core.Escalation, error) {
	return t.s.Reader().ListEscalations(f)
}
func (t tgStore) SetSessionTelegram(ctx context.Context, sessionID string, topicID, statusMsgID *int64) error {
	return t.s.WithTx(ctx, func(tx core.Tx) error {
		return tx.SetSessionTelegram(sessionID, topicID, statusMsgID)
	})
}

// buildTelegramBot constructs the forum bot for an enabled [telegram] config and
// verifies the token at boot (fail LOUD on a bad token, like other preflights).
func buildTelegramBot(ctx context.Context, cfg config.Config, eng *reconcile.Engine) (*telegram.Bot, error) {
	min, err := notify.ParseLevel(cfg.Telegram.MinLevel)
	if err != nil {
		return nil, err
	}
	client := telegram.NewClient(cfg.Telegram.Token, nil)
	me, err := client.GetMe(ctx)
	if err != nil {
		return nil, fmt.Errorf("token check (getMe) failed: %w", err)
	}
	log.Printf("arco: telegram forum mode ON: bot @%s, group %d, %d authorized user(s)",
		me.Username, cfg.Telegram.GroupID, len(cfg.Telegram.AllowedUserIDs))
	if len(cfg.Telegram.AllowedUserIDs) == 0 {
		log.Printf("arco: telegram: WARNING no allowed_user_ids — inbound is receive-nothing; button taps and console commands will be ignored")
	}
	// Durable chat history needs the console sentinel session (FK target for
	// General-topic chat). Create it before wiring the store into the bot.
	if err := ensureConsoleSession(ctx, eng.Store); err != nil {
		return nil, fmt.Errorf("console session: %w", err)
	}
	return telegram.New(telegram.Config{
		API:          client,
		Store:        tgStore{s: eng.Store},
		GroupID:      cfg.Telegram.GroupID,
		MinLevel:     min,
		Actions:      engineActions{eng: eng, estopPath: cfg.EStopPath()},
		Allowed:      cfg.Telegram.AllowedUserIDs,
		Redact:       eng.Redact,
		VMs:          vmLines(cfg),
		Registry:     buildRegistry(eng, vmLines(cfg)),
		ContextStore: contextStore{s: eng.Store},
	}), nil
}

// buildRegistry assembles the pluggable features from the engine — the
// composition root where a capability is wired once and bound to every surface.
// Adding a feature is one line here plus its constructor; nothing else changes.
func buildRegistry(eng *reconcile.Engine, vms []string) *feature.Registry {
	r := feature.NewRegistry()
	ledger := eng.Store.Reader()
	r.MustRegister(
		features.VMs(vms),
		features.Scan(eng),
		features.Peek(eng, eng.BrainReply),
		features.Workers(ledger),
		features.Sessions(ledger),
		features.Status(ledger, eng.Paused),
		features.Diff(eng, ledger),
		features.Kill(eng, ledger),
		features.Adopt(eng, func(ctx context.Context, ref string) (string, string, error) {
			res, err := eng.Adopt(ctx, ref)
			return res.WorkerID, res.SessionID, err
		}),
	)
	return r
}

// vmLines renders the attached fleet for the /vms command + chat context, from
// config: the [[vms]] entries, or the local herdr when none are configured.
func vmLines(cfg config.Config) []string {
	if len(cfg.VMs) == 0 {
		return []string{"local (default · this box, use_local_vm)"}
	}
	var out []string
	for _, v := range cfg.VMs {
		line := v.Name + " (host " + v.Host + ")"
		if v.Name == cfg.DefaultVM {
			line += " · default"
		}
		out = append(out, line)
	}
	if cfg.DefaultVM == "" {
		out = append(out, "local (default · this box)")
	}
	return out
}
