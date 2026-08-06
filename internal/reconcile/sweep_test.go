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

// Regression (qwen finding #1): a dispatched worker has no worktree, so HEAD
// must be keyed by WORKSPACE — otherwise progress-then-death is misclassified
// lost instead of completed_candidate.
func TestSweep_DispatchedWorkerProgressKeyedByWorkspace_Candidate(t *testing.T) {
	e, s, fake := newEngine(t)
	e.MissThreshold = 1
	res, err := e.Dispatch(context.Background(), "", "task", true) // no worktree set
	require.NoError(t, err)
	w0, _ := s.Reader().GetWorker(res.WorkerID)
	require.Empty(t, w0.Worktree, "dispatched worker has no worktree yet")

	fake.Heads["arco_"+res.WorkerID] = "advanced" // HEAD keyed by workspace
	fake.Agents = nil                             // then it vanished
	_, err = e.Sweep(context.Background())
	require.NoError(t, err)

	w, _ := s.Reader().GetWorker(res.WorkerID)
	require.Equal(t, core.WorkerCompletedCandidate, w.State, "progress-then-death must be candidate, not lost")
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

// The sweep correlates liveness by a worker's AgentRef (herdr pane_id captured
// at launch) when set, matching even if the agent reports a different workspace;
// it falls back to a Workspace match when AgentRef is empty (Prompt/Fake model).
func TestSweep_CorrelatesByAgentRef(t *testing.T) {
	e, s, fake := newEngine(t)
	e.MissThreshold = 1
	id := dispatchRunning(t, e)
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.BindAgentRef(id, "wZ:p1")
	}))

	// agent reports a DIFFERENT workspace but the matching ref → alive by ref
	fake.Agents = []core.AgentObs{{Ref: "wZ:p1", Workspace: "herdr-ws", Alive: true}}
	_, err := e.Sweep(context.Background())
	require.NoError(t, err)
	w, _ := s.Reader().GetWorker(id)
	require.Equal(t, core.WorkerRunning, w.State, "alive by AgentRef despite workspace mismatch")

	// ref gone → missed → finalized (MissThreshold=1, no head change → lost)
	fake.Agents = nil
	_, err = e.Sweep(context.Background())
	require.NoError(t, err)
	w, _ = s.Reader().GetWorker(id)
	require.Equal(t, core.WorkerLost, w.State, "ref absent → not alive → finalized")
}

// MED-4: a BLOCKED worker (e.g. parked at a billing wall) whose agent then
// advances HEAD and dies must TERMINALIZE, not wedge. completed_candidate is
// illegal from blocked, so finalize falls back to lost.
func TestSweep_BlockedAdvancedHeadThenGone_FinalizesLost(t *testing.T) {
	e, s, fake := newEngine(t)
	e.MissThreshold = 1
	id := mkRunning(t, e, s, "/wt/b", "base")
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		w, _ := tx.GetWorker(id)
		return tx.TransitionWorker(id, core.WorkerBlocked, w.Rev, core.Event{Kind: "state_change", WorkerID: id, SessionID: w.OwnerSession, Payload: "{}"})
	}))
	fake.Agents = nil                // agent gone
	fake.Heads["/wt/b"] = "advanced" // it committed before dying → would-be completed_candidate

	_, err := e.Sweep(context.Background())
	require.NoError(t, err)
	w, _ := s.Reader().GetWorker(id)
	require.Equal(t, core.WorkerLost, w.State, "blocked+advanced+gone terminalizes to lost, not wedged")
}

// MED-2: terminalizing a waiting worker expires its pending escalation, and a
// late answer can't resurrect the (now terminal) worker.
func TestSweep_TerminalizeExpiresEscalation_NoResurrect(t *testing.T) {
	e, s, fake := newEngine(t)
	e.MissThreshold = 1
	id := mkRunning(t, e, s, "/wt/w", "base")
	var escID string
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		w, _ := tx.GetWorker(id)
		if err := tx.TransitionWorker(id, core.WorkerWaitingForUser, w.Rev, core.Event{Kind: "state_change", WorkerID: id, SessionID: w.OwnerSession, Payload: "{}"}); err != nil {
			return err
		}
		var e2 error
		escID, e2 = tx.OpenEscalation(core.Escalation{WorkerID: id, SessionID: w.OwnerSession, Kind: "question", Action: "clarify?"})
		return e2
	}))
	fake.Agents = nil // agent died while waiting

	_, err := e.Sweep(context.Background())
	require.NoError(t, err)
	w, _ := s.Reader().GetWorker(id)
	require.True(t, w.State.Terminal(), "dead waiting worker terminalized")
	pend, _ := s.Reader().ListEscalations(core.EscalationFilter{Status: "pending", WorkerID: id})
	require.Empty(t, pend, "pending escalation expired on terminalize")

	// A late answer must not drive the terminal worker back to running.
	_ = s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.AnswerQuestion(escID, "go ahead", core.ScopeOnce, core.Event{Kind: "escalation_answered", WorkerID: id, SessionID: w.OwnerSession, Payload: "{}"})
	})
	w2, _ := s.Reader().GetWorker(id)
	require.True(t, w2.State.Terminal(), "a late answer must not resurrect a terminal worker")
}
