package reconcile

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/notify"
)

// A deny-listed attempt surfaces the danger confirm card; a redelivery of the
// same audit event (same sourceEventID) must not double-notify.
func TestNotify_AuditDenied_DangerCard_NoDoubleNotify(t *testing.T) {
	e, s, _ := newEngine(t)
	rec := &notify.Recorder{}
	e.Notify = rec
	id := mkRunning(t, e, s, "/wt/na1", "base")

	require.NoError(t, e.AuditDeniedAttempt(context.Background(), id, "rm_rf", "denied by hook", "evt-1"))

	c := lastCard(t, rec, 1)
	require.Equal(t, notify.LevelUrgent, c.Level)
	require.Contains(t, c.Title, id, "card must name the worker")
	require.Contains(t, c.Body, "deny-listed")

	require.NoError(t, e.AuditDeniedAttempt(context.Background(), id, "rm_rf", "denied by hook", "evt-1"))
	require.Len(t, rec.Cards(), 1, "a redelivered audit event must not re-notify")
}

// A stalled worker (StallN reached) surfaces the stall question card.
func TestNotify_Stall_OpenedCard(t *testing.T) {
	e, s, fake := newEngine(t)
	e.StallN = 1
	rec := &notify.Recorder{}
	e.Notify = rec
	id := mkRunning(t, e, s, "/wt/na2", "base")
	fake.Agents = []core.AgentObs{{Workspace: "arco_" + id, Alive: true}}
	fake.Heads["/wt/na2"] = "base" // alive, but HEAD never advances

	_, err := e.Sweep(context.Background())
	require.NoError(t, err)

	w, _ := s.Reader().GetWorker(id)
	require.Equal(t, core.WorkerBlocked, w.State)

	c := lastCard(t, rec, 1)
	require.Equal(t, notify.LevelUrgent, c.Level)
	require.Contains(t, c.Title, id, "card must name the worker")
	require.Contains(t, c.Body, "no progress")
}

// A human confirm decision surfaces an info card naming the worker + decision.
func TestNotify_DecideConfirm_AnsweredCard(t *testing.T) {
	e, s, _ := newEngine(t)
	rec := &notify.Recorder{}
	e.Notify = rec
	id := mkRunning(t, e, s, "/wt/na3", "base")
	w, err := s.Reader().GetWorker(id)
	require.NoError(t, err)

	var escID string
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		var err error
		escID, err = tx.OpenEscalation(core.Escalation{
			WorkerID: id, SessionID: w.OwnerSession, Kind: "confirm",
			QuestionClass: "clarify", ActionClass: core.ClassDanger, Tier: core.TierHighBlast,
			Action: "may we proceed",
		})
		return err
	}))

	require.NoError(t, e.DecideConfirm(context.Background(), escID, true, core.ScopeOnce))

	c := lastCard(t, rec, 1)
	require.Equal(t, notify.LevelInfo, c.Level)
	require.Contains(t, c.Title, "answered")
	require.Contains(t, c.Body, id, "card must name the worker")
	require.Contains(t, c.Body, "approved")
}
