package daemon

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/dinhlongviolin1/arco/internal/config"
	"github.com/dinhlongviolin1/arco/internal/core"
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

// tgStore adapts core.Store to telegram.Store: reads go through Reader(); the
// topic-id binding runs in a one-statement write tx.
type tgStore struct{ s core.Store }

func (t tgStore) GetSession(id string) (core.Session, error) { return t.s.Reader().GetSession(id) }
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
	return telegram.New(telegram.Config{
		API:      client,
		Store:    tgStore{s: eng.Store},
		GroupID:  cfg.Telegram.GroupID,
		MinLevel: min,
		Actions:  engineActions{eng: eng, estopPath: cfg.EStopPath()},
		Allowed:  cfg.Telegram.AllowedUserIDs,
		Redact:   eng.Redact,
	}), nil
}
