// Guideline tests for T2.1's engine seam: a pushed herdr agent-status change
// (from internal/herdrsock) feeds the SAME fusion path as polling.
// ApplyHerdrStatus resolves the worker by its stored AgentRef (pane_id) and
// applies a normal EventInput — push is a faster signal source, never a new
// authority (D1: hooks/push are fusion signals, the ledger reconcile is king).
package reconcile

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/ledger"
)

// setAgentRef stamps the herdr pane_id on a worker row (the launch path does
// this for real spawns; Prompt-model dispatch has no ref).
func setAgentRef(t *testing.T, s *ledger.Store, workerID, ref string) {
	t.Helper()
	_, err := s.DB().Exec(`UPDATE workers SET agent_ref=? WHERE id=?`, ref, workerID)
	require.NoError(t, err)
}

// A pushed "blocked" status transitions the running worker exactly like a
// polled observation would (fusion: blocked ⇒ WorkerBlocked).
func TestApplyHerdrStatus_BlockedTransitionsWorker(t *testing.T) {
	e, s, _ := newEngine(t)
	id := mkRunning(t, e, s, "/wt/hp1", "base")
	setAgentRef(t, s, id, "wB:p7")

	require.NoError(t, e.ApplyHerdrStatus(context.Background(), "wB:p7", "blocked"))

	w, err := s.Reader().GetWorker(id)
	require.NoError(t, err)
	require.Equal(t, core.WorkerBlocked, w.State)
}

// A pushed "working" status for a running worker is a no-op transition (the
// signal agrees with the ledger) — but it must not error.
func TestApplyHerdrStatus_WorkingKeepsRunning(t *testing.T) {
	e, s, _ := newEngine(t)
	id := mkRunning(t, e, s, "/wt/hp2", "base")
	setAgentRef(t, s, id, "wB:p8")

	require.NoError(t, e.ApplyHerdrStatus(context.Background(), "wB:p8", "working"))

	w, err := s.Reader().GetWorker(id)
	require.NoError(t, err)
	require.Equal(t, core.WorkerRunning, w.State)
}

// A pane arco never launched (no worker carries that AgentRef) is silently
// ignored: the operator's own panes must not error-spam the daemon, and push
// must NEVER invent a worker row.
func TestApplyHerdrStatus_UnknownPaneIgnored(t *testing.T) {
	e, s, _ := newEngine(t)
	mkRunning(t, e, s, "/wt/hp3", "base") // an unrelated worker exists

	require.NoError(t, e.ApplyHerdrStatus(context.Background(), "wZ:p99", "working"))

	ws, err := s.Reader().ListWorkers(core.WorkerFilter{})
	require.NoError(t, err)
	require.Len(t, ws, 1, "push must not invent workers")
}

// A terminal worker is not resurrected by a late push frame (the queued event
// arrived after the sweep already finalized the worker).
func TestApplyHerdrStatus_TerminalWorkerNotResurrected(t *testing.T) {
	e, s, _ := newEngine(t)
	id := mkRunning(t, e, s, "/wt/hp4", "base")
	setAgentRef(t, s, id, "wB:p9")
	require.NoError(t, e.KillWorker(context.Background(), id))
	w, err := s.Reader().GetWorker(id)
	require.NoError(t, err)
	require.Equal(t, core.WorkerKilled, w.State)

	_ = e.ApplyHerdrStatus(context.Background(), "wB:p9", "working") // stale frame

	w, err = s.Reader().GetWorker(id)
	require.NoError(t, err)
	require.Equal(t, core.WorkerKilled, w.State, "a late push frame must not resurrect a killed worker")
}
