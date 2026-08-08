package reconcile

import (
	"context"

	"github.com/dinhlongviolin1/arco/internal/core"
)

// ApplyHerdrStatus feeds one pushed herdr agent-status change (from
// internal/herdrsock) into the SAME fusion path as polling: resolve the worker
// whose AgentRef is the pane, then ApplyEvent a normal EventInput. Push is a
// faster signal source, never a new authority (D1) — so an unknown pane is a
// silent nil (the operator's own panes flow through the same subscription and
// must never error-spam or invent a worker row), and a stale frame against a
// terminal worker is swallowed the same way the sweep's paths are: ApplyEvent
// already keeps the current state on an illegal transition (and on a rev race)
// and returns nil, so no special-casing here.
func (e *Engine) ApplyHerdrStatus(ctx context.Context, paneID, status string) error {
	if paneID == "" || status == "" {
		return nil
	}
	// Reader ListWorkers scan is fine — the fleet is small.
	ws, err := e.Store.Reader().ListWorkers(core.WorkerFilter{})
	if err != nil {
		return err
	}
	for _, w := range ws {
		if w.AgentRef == "" || w.AgentRef != paneID {
			continue
		}
		return e.ApplyEvent(ctx, EventInput{
			WorkerID: w.ID, HerdrState: status,
			Alive: status != "done", // done is the only terminal agent_status
		})
	}
	return nil
}
