package reconcile

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/ledger"
)

// mkRunning dispatches a worker and forces it to running with a known worktree.
func mkRunning(t *testing.T, e *Engine, s *ledger.Store, worktree, head string) string {
	t.Helper()
	res, err := e.Dispatch(context.Background(), "", "task", true)
	require.NoError(t, err)
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.ObserveWorker(res.WorkerID, core.WorkerObservation{HeadCommit: head})
	}))
	// set the worktree so the sweep can query GitHeads for it
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.ObserveWorker(res.WorkerID, core.WorkerObservation{})
	}))
	// worktree is not settable via ObserveWorker; patch directly for the test
	_, err = s.DB().Exec(`UPDATE workers SET worktree=? WHERE id=?`, worktree, res.WorkerID)
	require.NoError(t, err)
	return res.WorkerID
}

func TestSweep_AliveResetsMissAndRecordsHead(t *testing.T) {
	e, s, fake := newEngine(t)
	id := mkRunning(t, e, s, "/wt/a", "base")
	fake.Agents = []core.AgentObs{{Workspace: "arco_" + id, Alive: true}}
	fake.Heads["/wt/a"] = "newhead"

	_, err := e.Sweep(context.Background())
	require.NoError(t, err)
	w, _ := s.Reader().GetWorker(id)
	require.Equal(t, core.WorkerRunning, w.State) // liveness alone doesn't change state
	require.Equal(t, "newhead", w.HeadCommit)     // HEAD recorded
}

func TestSweep_SuspectBelowThresholdNoChange(t *testing.T) {
	e, s, fake := newEngine(t)
	e.MissThreshold = 3
	id := mkRunning(t, e, s, "/wt/b", "base")
	fake.Agents = nil // not observed → missing

	for i := 0; i < 2; i++ { // below threshold
		_, err := e.Sweep(context.Background())
		require.NoError(t, err)
	}
	w, _ := s.Reader().GetWorker(id)
	require.Equal(t, core.WorkerRunning, w.State) // still suspect, not finalized
}

func TestSweep_ConfirmedMissingNoProgress_Lost(t *testing.T) {
	e, s, fake := newEngine(t)
	e.MissThreshold = 2
	id := mkRunning(t, e, s, "/wt/c", "base")
	fake.Agents = nil // missing every sweep, no HEAD change

	for i := 0; i < 2; i++ {
		_, err := e.Sweep(context.Background())
		require.NoError(t, err)
	}
	w, _ := s.Reader().GetWorker(id)
	require.Equal(t, core.WorkerLost, w.State)
}

func TestSweep_ConfirmedMissingWithProgress_Candidate(t *testing.T) {
	e, s, fake := newEngine(t)
	e.MissThreshold = 2
	id := mkRunning(t, e, s, "/wt/d", "base")
	fake.Agents = nil
	fake.Heads["/wt/d"] = "advanced" // HEAD moved before it vanished

	for i := 0; i < 2; i++ {
		_, err := e.Sweep(context.Background())
		require.NoError(t, err)
	}
	w, _ := s.Reader().GetWorker(id)
	require.Equal(t, core.WorkerCompletedCandidate, w.State)
}

func TestSweep_SkipsTerminalWorkers(t *testing.T) {
	e, s, fake := newEngine(t)
	id := mkRunning(t, e, s, "/wt/e", "base")
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		w, _ := tx.GetWorker(id)
		return tx.TransitionWorker(id, core.WorkerKilled, w.Rev, core.Event{Kind: "kill_done", WorkerID: id})
	}))
	fake.Agents = nil
	r, err := e.Sweep(context.Background())
	require.NoError(t, err)
	require.Equal(t, 0, r.Observed) // terminal worker not observed
}

func TestRecover_StartingWorkerResolvedByLiveness(t *testing.T) {
	e, s, fake := newEngine(t)
	// Simulate a crash mid-dispatch: a worker stuck in `starting`.
	stuck := "01AAAAAAAAAAAAAAAAAAAAAAAA"
	sess := "01BBBBBBBBBBBBBBBBBBBBBBBB"
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		if err := tx.CreateSession(core.Session{ID: sess, Status: core.SessionActive, Kind: core.SessionKindWork}); err != nil {
			return err
		}
		return tx.CreateWorker(core.Worker{ID: stuck, OwnerSession: sess, State: core.WorkerStarting, Workspace: "arco_stuck"})
	}))

	// process not found → failed (never double-spawn)
	fake.Agents = nil
	require.NoError(t, e.Recover(context.Background()))
	w, _ := s.Reader().GetWorker(stuck)
	require.Equal(t, core.WorkerFailed, w.State)
}

func TestRecover_StartingWorkerAliveAdopted(t *testing.T) {
	e, s, fake := newEngine(t)
	stuck := "01CCCCCCCCCCCCCCCCCCCCCCCC"
	sess := "01DDDDDDDDDDDDDDDDDDDDDDDD"
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		if err := tx.CreateSession(core.Session{ID: sess, Status: core.SessionActive, Kind: core.SessionKindWork}); err != nil {
			return err
		}
		return tx.CreateWorker(core.Worker{ID: stuck, OwnerSession: sess, State: core.WorkerStarting, Workspace: "arco_live"})
	}))
	fake.Agents = []core.AgentObs{{Workspace: "arco_live", Alive: true}}
	require.NoError(t, e.Recover(context.Background()))
	w, _ := s.Reader().GetWorker(stuck)
	require.Equal(t, core.WorkerRunning, w.State) // adopted
}
