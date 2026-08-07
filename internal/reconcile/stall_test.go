package reconcile

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
)

// Guideline tests (rev7/T1.3): an ALIVE worker whose git HEAD does not advance
// for StallN consecutive sweeps is a stall → transition running→blocked and
// open an escalation, exactly once. Progress (HEAD advance) resets the counter.
// StallN=0 disables. Do not weaken these asserts.

func pendingFor(t *testing.T, s interface {
	Reader() core.Reader
}, worker string) []core.Escalation {
	t.Helper()
	es, err := s.Reader().ListEscalations(core.EscalationFilter{Status: "pending"})
	require.NoError(t, err)
	var out []core.Escalation
	for _, e := range es {
		if e.WorkerID == worker {
			out = append(out, e)
		}
	}
	return out
}

func TestSweep_Stall_NMinus1NoOp_NTriggersOnce(t *testing.T) {
	e, s, fake := newEngine(t)
	e.StallN = 3
	id := mkRunning(t, e, s, "/wt/stall", "base")
	fake.Agents = []core.AgentObs{{Workspace: "arco_" + id, Alive: true}}
	fake.Heads["/wt/stall"] = "base" // alive, HEAD never advances

	// N-1 sweeps: counter climbs, no state change, no escalation.
	for i := 1; i <= 2; i++ {
		_, err := e.Sweep(context.Background())
		require.NoError(t, err)
		w, _ := s.Reader().GetWorker(id)
		require.Equal(t, core.WorkerRunning, w.State, "sweep %d must not block yet", i)
		require.Equal(t, i, w.StallCount, "stall_count must persist in the ledger")
		require.Empty(t, pendingFor(t, s, id))
	}

	// Nth sweep: blocked + escalation.
	_, err := e.Sweep(context.Background())
	require.NoError(t, err)
	w, _ := s.Reader().GetWorker(id)
	require.Equal(t, core.WorkerBlocked, w.State)
	esc := pendingFor(t, s, id)
	require.Len(t, esc, 1)

	// Another stalled sweep: still exactly one escalation, still blocked, no error.
	_, err = e.Sweep(context.Background())
	require.NoError(t, err)
	w, _ = s.Reader().GetWorker(id)
	require.Equal(t, core.WorkerBlocked, w.State)
	require.Len(t, pendingFor(t, s, id), 1)
}

func TestSweep_Stall_ProgressResetsCounter(t *testing.T) {
	e, s, fake := newEngine(t)
	e.StallN = 3
	id := mkRunning(t, e, s, "/wt/prog", "base")
	fake.Agents = []core.AgentObs{{Workspace: "arco_" + id, Alive: true}}
	fake.Heads["/wt/prog"] = "base"

	for i := 0; i < 2; i++ {
		_, err := e.Sweep(context.Background())
		require.NoError(t, err)
	}
	w, _ := s.Reader().GetWorker(id)
	require.Equal(t, 2, w.StallCount)

	// HEAD advances → progress → counter resets, worker stays running.
	fake.Heads["/wt/prog"] = "head2"
	_, err := e.Sweep(context.Background())
	require.NoError(t, err)
	w, _ = s.Reader().GetWorker(id)
	require.Equal(t, core.WorkerRunning, w.State)
	require.Equal(t, 0, w.StallCount)

	// Two more stalled sweeps: counter restarted from zero → still running.
	for i := 0; i < 2; i++ {
		_, err := e.Sweep(context.Background())
		require.NoError(t, err)
	}
	w, _ = s.Reader().GetWorker(id)
	require.Equal(t, core.WorkerRunning, w.State)
	require.Equal(t, 2, w.StallCount)
	require.Empty(t, pendingFor(t, s, id))
}

func TestSweep_Stall_DisabledByZero(t *testing.T) {
	e, s, fake := newEngine(t)
	e.StallN = 0
	id := mkRunning(t, e, s, "/wt/off", "base")
	fake.Agents = []core.AgentObs{{Workspace: "arco_" + id, Alive: true}}
	fake.Heads["/wt/off"] = "base"

	for i := 0; i < 5; i++ {
		_, err := e.Sweep(context.Background())
		require.NoError(t, err)
	}
	w, _ := s.Reader().GetWorker(id)
	require.Equal(t, core.WorkerRunning, w.State)
	require.Empty(t, pendingFor(t, s, id))
}

// A worker with no observable HEAD (prompt-model, no worktree registered in the
// fake) must never accrue stall: absence of the signal is not "no progress".
func TestSweep_Stall_NoHeadSignalNoBump(t *testing.T) {
	e, s, fake := newEngine(t)
	e.StallN = 2
	id := mkRunning(t, e, s, "/wt/nohead", "base")
	fake.Agents = []core.AgentObs{{Workspace: "arco_" + id, Alive: true}}
	// fake.Heads deliberately empty → GitHeads returns no entry for /wt/nohead

	for i := 0; i < 4; i++ {
		_, err := e.Sweep(context.Background())
		require.NoError(t, err)
	}
	w, _ := s.Reader().GetWorker(id)
	require.Equal(t, core.WorkerRunning, w.State)
	require.Equal(t, 0, w.StallCount)
	require.Empty(t, pendingFor(t, s, id))
}
