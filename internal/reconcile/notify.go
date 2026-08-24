package reconcile

import (
	"log"
	"unicode/utf8"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/notify"
)

// notifyCard delivers one decision card POST-COMMIT, best-effort. A nil
// sender (notifications unconfigured) drops the card; a send error is logged
// and swallowed — a notification outage must never fail a reconcile path.
// Async: a push is network I/O (shoutrrr HTTP), so it must never block a sweep
// tick or an API response path (same reasoning as deliverDecision).
// sessionID is the session the card concerns: this single chokepoint enforces
// the D9 ActNotify gate (a manual-mode session never pings the phone). The
// drop is synchronous, before the goroutine, so a gated card can never race a
// caller's assertion of silence.
func (e *Engine) notifyCard(sessionID string, c notify.Card) {
	if e.Notify == nil {
		return
	}
	if !e.sessionMode(sessionID).Allows(core.ActNotify) {
		return
	}
	// Stamp the concerned session so a topic-routing sender (Telegram forum) can
	// deliver into the right per-session thread; the generic shoutrrr sender
	// ignores it. FormatEscalation may already have set it — this is the single
	// chokepoint that guarantees it for every card kind.
	c.SessionID = sessionID
	go func() {
		if err := e.Notify.Send(e.bg(), c); err != nil {
			e.meterNotifyFailure()
			log.Printf("arco: notify: send failed: %v", err)
		}
	}()
}

// pendingEscForCard returns a worker's pending escalation, so a decision card
// can carry the routing metadata (escalation id + kind) a topic-aware notifier
// (Telegram forum) needs to attach answer buttons. Best-effort: a read failure
// or no pending row yields ok=false and the card still sends (button-less). A
// worker holds at most one pending escalation (the open path gates on that), so
// the first row is the right one.
func (e *Engine) pendingEscForCard(workerID string) (core.Escalation, bool) {
	list, err := e.Store.Reader().ListEscalations(core.EscalationFilter{WorkerID: workerID, Status: "pending"})
	if err != nil || len(list) == 0 {
		return core.Escalation{}, false
	}
	return list[0], true
}

// taskTail is the tail of a worker task for decision cards: the last 120
// characters, rune-aligned so a multi-byte rune is never split.
func taskTail(task string) string {
	const n = 120
	if len(task) <= n {
		return task
	}
	cut := len(task) - n
	for cut < len(task) && !utf8.RuneStart(task[cut]) {
		cut++
	}
	return task[cut:]
}
