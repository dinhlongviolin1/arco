package reconcile

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
)

// hasEvent reports whether the worker's recent events include kind.
func hasEvent(t *testing.T, e *Engine, workerID, kind string) bool {
	t.Helper()
	evs, err := e.Store.Reader().RecentWorkerEvents(workerID, 50)
	require.NoError(t, err)
	for _, ev := range evs {
		if ev.Kind == kind {
			return true
		}
	}
	return false
}

func TestRedeliver_RePromptsStrandedWorker(t *testing.T) {
	e, s, fake := newEngine(t)
	id := mkRunning(t, e, s, "/wt/rd", "base")
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.BindLaunch(id, "/wt/rd", "base", "wR:p1", "term_wR")
	}))

	require.NoError(t, e.RedeliverInitialTask(context.Background(), id))

	// the original task is re-prompted to the captured pane
	p, ok := promptTo(fake.Prompts(), "wR:p1")
	require.True(t, ok, "task re-delivered to the worker's pane")
	require.Contains(t, p.Text, "task", "the worker's ORIGINAL task is re-delivered")
	// the dangling intent is resolved + an audit marker recorded
	require.True(t, hasEvent(t, e, id, "prompt_delivered"), "prompt_delivered recorded")
	require.True(t, hasEvent(t, e, id, "task_redelivered"), "task_redelivered audit event recorded")
	// the worker stays running (redeliver never changes state)
	require.Equal(t, core.WorkerRunning, mustWorker(t, s, id).State)
}

func TestRedeliver_RefusesBusyAgent(t *testing.T) {
	for _, status := range []string{"working", "blocked"} {
		t.Run(status, func(t *testing.T) {
			e, s, fake := newEngine(t)
			id := mkRunning(t, e, s, "/wt/rd", "base")
			require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
				return tx.BindLaunch(id, "/wt/rd", "base", "wR:p1", "term_wR")
			}))
			fake.Statuses = map[string]string{"wR:p1": status}

			err := e.RedeliverInitialTask(context.Background(), id)
			require.Error(t, err, "must refuse to re-prompt a %s agent", status)
			require.ErrorIs(t, err, core.ErrAgentBusy)
			require.Empty(t, fake.Killed())
			for _, p := range fake.Prompts() {
				require.NotEqual(t, "wR:p1", p.Workspace, "no prompt delivered to a %s agent", status)
			}
		})
	}
}

// A non-busy status (idle/done/unknown/"") passes the gate — the operator has
// judged the pane safe; only working/blocked are machine-refused.
func TestRedeliver_NonBusyStatusProceeds(t *testing.T) {
	for _, status := range []string{"idle", "done", "unknown", ""} {
		t.Run("status="+status, func(t *testing.T) {
			e, s, fake := newEngine(t)
			id := mkRunning(t, e, s, "/wt/rd", "base")
			require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
				return tx.BindLaunch(id, "/wt/rd", "base", "wR:p1", "term_wR")
			}))
			fake.Statuses = map[string]string{"wR:p1": status}
			require.NoError(t, e.RedeliverInitialTask(context.Background(), id))
			_, ok := promptTo(fake.Prompts(), "wR:p1")
			require.True(t, ok, "a non-busy agent is re-prompted")
		})
	}
}

// The HEAD-progress guard: if the worker already committed past its base, the task
// almost certainly ran — re-prompting would double-execute. Refuse (ledger-only).
func TestRedeliver_RefusesWhenHeadAdvanced(t *testing.T) {
	e, s, fake := newEngine(t)
	id := mkRunning(t, e, s, "/wt/rd", "base")
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		if err := tx.BindLaunch(id, "/wt/rd", "base", "wR:p1", "term_wR"); err != nil {
			return err
		}
		// agent made progress: HEAD advanced past base
		return tx.ObserveWorker(id, core.WorkerObservation{HeadCommit: "advanced"})
	}))

	err := e.RedeliverInitialTask(context.Background(), id)
	require.Error(t, err, "must refuse when the worker already committed progress")
	require.ErrorIs(t, err, core.ErrIllegalTransition)
	_, delivered := promptTo(fake.Prompts(), "wR:p1")
	require.False(t, delivered, "no re-prompt of a task that already ran")
	require.False(t, hasEvent(t, e, id, "redeliver_intent"), "a refusal leaves no dangling intent")
}

func TestRedeliver_RefusesNonRunning(t *testing.T) {
	e, s, _ := newEngine(t)
	id := mkRunning(t, e, s, "/wt/rd", "base")
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		w, _ := tx.GetWorker(id)
		return tx.TransitionWorker(id, core.WorkerPaused, w.Rev, core.Event{Kind: "state_change", WorkerID: id, SessionID: w.OwnerSession, Payload: "{}"})
	}))
	err := e.RedeliverInitialTask(context.Background(), id)
	require.ErrorIs(t, err, core.ErrIllegalTransition, "only a running worker can be redelivered")
}

func TestRedeliver_RefusesPoolOwned(t *testing.T) {
	e, s, _ := newEngine(t)
	id := mkRunning(t, e, s, "/wt/rd", "base")
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error { return tx.ReleaseWorker(id, "cli") }))
	err := e.RedeliverInitialTask(context.Background(), id)
	require.ErrorIs(t, err, core.ErrProtectedPool, "a pool-owned worker is not redeliverable")
}

func TestRedeliver_PromptFailureRecorded(t *testing.T) {
	e, s, fake := newEngine(t)
	id := mkRunning(t, e, s, "/wt/rd", "base")
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.BindLaunch(id, "/wt/rd", "base", "wR:p1", "term_wR")
	}))
	fake.PromptErr = errors.New("prompt boom")

	err := e.RedeliverInitialTask(context.Background(), id)
	require.Error(t, err, "a delivery failure surfaces to the operator")
	require.True(t, hasEvent(t, e, id, "error"), "the failure is recorded in the ledger")
	require.False(t, hasEvent(t, e, id, "prompt_delivered"), "no delivery marker on failure")
}
