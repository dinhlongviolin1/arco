package reconcile

import (
	"log"
	"unicode/utf8"

	"github.com/dinhlongviolin1/arco/internal/notify"
)

// notifyCard delivers one decision card POST-COMMIT, best-effort. A nil
// sender (notifications unconfigured) drops the card; a send error is logged
// and swallowed — a notification outage must never fail a reconcile path.
// Async: a push is network I/O (shoutrrr HTTP), so it must never block a sweep
// tick or an API response path (same reasoning as deliverDecision).
func (e *Engine) notifyCard(c notify.Card) {
	if e.Notify == nil {
		return
	}
	go func() {
		if err := e.Notify.Send(e.bg(), c); err != nil {
			log.Printf("arco: notify: send failed: %v", err)
		}
	}()
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
