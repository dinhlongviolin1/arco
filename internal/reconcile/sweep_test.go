package reconcile

import (
	"context"
	"testing"
	"time"

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

// LOW-5 (whole-system audit): a completed_candidate worker has no legal edge to
// lost, so finalize no-ops when its (expectedly-gone) agent is missing. The miss
// counter must still be RESET each sweep — else it re-bumps forever and grows the
// in-memory misses map without bound.
func TestSweep_UnfinalizeableDoesNotLeakMisses(t *testing.T) {
	e, s, fake := newEngine(t)
	e.MissThreshold = 1
	id := mkRunning(t, e, s, "/wt/cc", "base")
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		w, _ := tx.GetWorker(id)
		return tx.TransitionWorker(id, core.WorkerCompletedCandidate, w.Rev, core.Event{Kind: "state_change", WorkerID: id, SessionID: w.OwnerSession, Payload: "{}"})
	}))
	fake.Agents = nil // agent gone — expected once a worker is completed_candidate

	for i := 0; i < 3; i++ {
		_, err := e.Sweep(context.Background())
		require.NoError(t, err)
	}
	require.Equal(t, core.WorkerCompletedCandidate, mustWorker(t, s, id).State, "stays candidate — no legal edge to lost")
	e.mu.Lock()
	_, present := e.misses[id]
	e.mu.Unlock()
	require.False(t, present, "miss counter reset each sweep, not leaked unboundedly")
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

// MED-1: a pending escalation older than EscalationTimeout is expired and its
// waiting worker auto-paused (so a worker can't wait on a human forever).
func TestSweep_EscalationTimeout_ExpiresAndPauses(t *testing.T) {
	base := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	clk := &settableClock{}
	clk.set(base)
	e, s, _ := newEngine(t)
	s.SetClock(clk.now)
	e.EscalationTimeout = 30 * time.Minute
	id := mkRunning(t, e, s, "/wt/e", "base")
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		w, _ := tx.GetWorker(id)
		if err := tx.TransitionWorker(id, core.WorkerWaitingForUser, w.Rev, core.Event{Kind: "state_change", WorkerID: id, SessionID: w.OwnerSession, Payload: "{}"}); err != nil {
			return err
		}
		_, err := tx.OpenEscalation(core.Escalation{WorkerID: id, SessionID: w.OwnerSession, Kind: "question", Action: "clarify?"})
		return err
	}))

	// not yet timed out
	clk.set(base.Add(20 * time.Minute))
	res, err := e.Sweep(context.Background())
	require.NoError(t, err)
	require.Equal(t, 0, res.EscalationsTimedOut)
	require.Equal(t, core.WorkerWaitingForUser, mustWorker(t, s, id).State)

	// past the timeout → expired + worker paused
	clk.set(base.Add(31 * time.Minute))
	res, err = e.Sweep(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, res.EscalationsTimedOut)
	require.Equal(t, core.WorkerPaused, mustWorker(t, s, id).State)
	pend, _ := s.Reader().ListEscalations(core.EscalationFilter{Status: "pending", WorkerID: id})
	require.Empty(t, pend, "timed-out escalation expired")
}

// MED-3: an operator kill terminalizes the worker, expires its pending escalation,
// and stops the agent (VM.Kill on the pane ref).
func TestKillWorker_TerminatesAndStopsAgent(t *testing.T) {
	e, s, fake := newEngine(t)
	id := mkRunning(t, e, s, "/wt/k", "base")
	// give it a pane ref + a pending escalation
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		if err := tx.BindLaunch(id, "/wt/k", "base", "wZ:p1", "term_K"); err != nil {
			return err
		}
		w, _ := tx.GetWorker(id)
		if err := tx.TransitionWorker(id, core.WorkerWaitingForUser, w.Rev, core.Event{Kind: "state_change", WorkerID: id, SessionID: w.OwnerSession, Payload: "{}"}); err != nil {
			return err
		}
		_, err := tx.OpenEscalation(core.Escalation{WorkerID: id, SessionID: w.OwnerSession, Kind: "question", Action: "q?"})
		return err
	}))

	require.NoError(t, e.KillWorker(context.Background(), id))
	w, _ := s.Reader().GetWorker(id)
	require.Equal(t, core.WorkerKilled, w.State)
	pend, _ := s.Reader().ListEscalations(core.EscalationFilter{Status: "pending", WorkerID: id})
	require.Empty(t, pend, "kill expires the pending escalation")
	require.Contains(t, fake.Killed(), "wZ:p1", "kill stops the agent at its pane ref")

	// idempotent: killing an already-terminal worker is a no-op (no error)
	require.NoError(t, e.KillWorker(context.Background(), id))
}

// seedTerminalWithAgent makes worker id terminal (killed) with a captured pane
// ref + a recorded terminal_id (BootID) — the state left by a worker that ran,
// was observed alive once, then terminalized while its pane lingered.
func seedTerminalWithAgent(t *testing.T, s *ledger.Store, id, ref, termID string) {
	t.Helper()
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		// capture ref + stable identity (terminal_id) at launch → arms the guard
		if err := tx.BindLaunch(id, "/wt/o", "base", ref, termID); err != nil {
			return err
		}
		w, _ := tx.GetWorker(id)
		return tx.TransitionWorker(id, core.WorkerKilled, w.Rev, core.Event{Kind: "state_change", WorkerID: id, SessionID: w.OwnerSession, Payload: "{}"})
	}))
}

// MED-3 (sweep): the sweep stops an agent still alive on a TERMINAL worker's pane
// — the kill crash-orphan qwen flagged (KillWorker's commit landed, its best-
// effort VM.Kill didn't) or any lingering terminal pane. Reaping is idempotent
// (a closed pane leaves ListAgents so it isn't re-targeted).
func TestSweep_ReapsOrphanedTerminalAgent(t *testing.T) {
	e, s, fake := newEngine(t)
	id := mkRunning(t, e, s, "/wt/o", "base")
	seedTerminalWithAgent(t, s, id, "wZ:p1", "term_A")
	// live agent on the pane with the SAME terminal_id → positively ours
	fake.Agents = []core.AgentObs{{Ref: "wZ:p1", Workspace: "arco_" + id, BootID: "term_A", Alive: true}}

	res, err := e.Sweep(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, res.AgentsReaped, "orphaned terminal agent stopped")
	require.Contains(t, fake.Killed(), "wZ:p1")
	require.Equal(t, 0, res.Observed, "a terminal worker is not in the liveness loop")

	// idempotent: pane closed → gone from ListAgents → not re-reaped
	fake.Agents = nil
	res, err = e.Sweep(context.Background())
	require.NoError(t, err)
	require.Equal(t, 0, res.AgentsReaped)
}

// The orphan reaper must NOT stop a live (non-terminal) worker's agent — only a
// terminal worker's pane is an orphan.
func TestSweep_DoesNotReapLiveWorkerAgent(t *testing.T) {
	e, s, fake := newEngine(t)
	id := mkRunning(t, e, s, "/wt/p", "base")
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.BindLaunch(id, "/wt/p", "base", "wY:p1", "term_A")
	}))
	fake.Agents = []core.AgentObs{{Ref: "wY:p1", Workspace: "arco_" + id, BootID: "term_A", Alive: true}}

	res, err := e.Sweep(context.Background())
	require.NoError(t, err)
	require.Equal(t, 0, res.AgentsReaped, "a running worker's agent is not an orphan")
	require.Empty(t, fake.Killed())
}

// MED-3 auto-kill-on-pause: the sweep reclaims a PAUSED worker's idle agent (pure
// quota leak — its worktree is preserved, resume is via relaunch) using the same
// identity-strict reaper, and does NOT liveness-finalize the paused worker.
func TestSweep_ReapsPausedWorkerAgent(t *testing.T) {
	e, s, fake := newEngine(t)
	id := mkRunning(t, e, s, "/wt/pz", "base")
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		if err := tx.BindLaunch(id, "/wt/pz", "base", "wP:p1", "term_P"); err != nil {
			return err
		}
		w, _ := tx.GetWorker(id)
		return tx.TransitionWorker(id, core.WorkerPaused, w.Rev, core.Event{Kind: "state_change", WorkerID: id, SessionID: w.OwnerSession, Payload: "{}"})
	}))
	fake.Agents = []core.AgentObs{{Ref: "wP:p1", Workspace: "arco_" + id, BootID: "term_P", Alive: true}}

	res, err := e.Sweep(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, res.AgentsReaped, "a paused worker's idle agent is reclaimed")
	require.Contains(t, fake.Killed(), "wP:p1")
	require.Equal(t, core.WorkerPaused, mustWorker(t, s, id).State, "still paused (worktree preserved), not finalized")
	require.Equal(t, 0, res.Observed, "a paused worker is not in the liveness loop")
}

// Regression (opus review): a paused worker WITH a pending escalation keeps its
// agent — an operator approval re-prompts the SAME pane (deliverDecision
// reconnect), so reaping it would silently discard the approval. It is NOT
// reclaimed, and stays liveness-tracked.
func TestSweep_PausedWithPendingEscalationNotReaped(t *testing.T) {
	e, s, fake := newEngine(t)
	id := mkRunning(t, e, s, "/wt/pz3", "base")
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		if err := tx.BindLaunch(id, "/wt/pz3", "base", "wD:p1", "term_D"); err != nil {
			return err
		}
		w, _ := tx.GetWorker(id)
		// audit-deny style: running worker paused WITH a pending danger confirm
		if err := tx.TransitionWorker(id, core.WorkerPaused, w.Rev, core.Event{Kind: "state_change", WorkerID: id, SessionID: w.OwnerSession, Payload: "{}"}); err != nil {
			return err
		}
		_, err := tx.OpenEscalation(core.Escalation{WorkerID: id, SessionID: w.OwnerSession, Kind: "confirm", Action: "danger?"})
		return err
	}))
	fake.Agents = []core.AgentObs{{Ref: "wD:p1", Workspace: "arco_" + id, BootID: "term_D", Alive: true}}

	res, err := e.Sweep(context.Background())
	require.NoError(t, err)
	require.Equal(t, 0, res.AgentsReaped, "a paused worker with a pending escalation keeps its agent")
	require.Empty(t, fake.Killed(), "an approval will re-prompt this pane — must not close it")
	require.Equal(t, 1, res.Observed, "still liveness-tracked (its agent should be alive)")
}

// A paused, agent-less worker must NOT be finalized to lost — its absent agent is
// expected (auto-killed on pause), not a liveness death (the coupling behind why
// auto-kill-on-pause and excluding-paused-from-liveness are one change).
func TestSweep_PausedWorkerNotFinalized(t *testing.T) {
	e, s, fake := newEngine(t)
	e.MissThreshold = 1
	id := mkRunning(t, e, s, "/wt/pz2", "base")
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		w, _ := tx.GetWorker(id)
		return tx.TransitionWorker(id, core.WorkerPaused, w.Rev, core.Event{Kind: "state_change", WorkerID: id, SessionID: w.OwnerSession, Payload: "{}"})
	}))
	fake.Agents = nil // agent already gone (auto-killed on pause)
	for i := 0; i < 3; i++ {
		_, err := e.Sweep(context.Background())
		require.NoError(t, err)
	}
	require.Equal(t, core.WorkerPaused, mustWorker(t, s, id).State, "a paused, agent-less worker stays paused, not lost")
}

// Regression (opus+qwen review): the "empty-at-birth" poisoning window. A worker
// whose launch-capture missed (no boot_id) must NOT absorb an observed agent's
// terminal_id via the liveness path — else a stranger on a recycled pane becomes
// the worker's recorded identity and the reaper later destructively closes that
// innocent agent. Observation only CONFIRMS an identity; it never ESTABLISHES one.
func TestSweep_LivenessDoesNotEstablishIdentity(t *testing.T) {
	e, s, fake := newEngine(t)
	e.MissThreshold = 5
	id := mkRunning(t, e, s, "/wt/x", "base")
	// launch-capture missed: ref bound, but identity ("") never captured at birth
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.BindLaunch(id, "/wt/x", "base", "wQ:p1", "")
	}))
	// a STRANGER now holds that pane (herdr recycled it), with its OWN terminal_id
	fake.Agents = []core.AgentObs{{Ref: "wQ:p1", Workspace: "arco_" + id, BootID: "term_STRANGER", Alive: true}}

	_, err := e.Sweep(context.Background())
	require.NoError(t, err)
	require.Empty(t, mustWorker(t, s, id).BootID,
		"liveness must NOT stamp a stranger's identity onto an empty-at-birth worker")

	// terminalize it; the reaper must decline (identity was never established)
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		w, _ := tx.GetWorker(id)
		return tx.TransitionWorker(id, core.WorkerKilled, w.Rev, core.Event{Kind: "state_change", WorkerID: id, SessionID: w.OwnerSession, Payload: "{}"})
	}))
	res, err := e.Sweep(context.Background())
	require.NoError(t, err)
	require.Equal(t, 0, res.AgentsReaped, "no positive identity → never reap")
	require.Empty(t, fake.Killed(), "the innocent recycled-pane agent must survive")
}

// Regression (opus review): the reaper must NEVER close a workspace it can't
// positively identify. If our agent died and herdr recycled the pane_id to an
// UNRELATED live agent (different terminal_id), reaping by ref alone would wrongly
// close that innocent workspace. A terminal_id mismatch (or an unknown identity on
// either side) must skip.
func TestSweep_DoesNotReapRecycledPane(t *testing.T) {
	e, s, fake := newEngine(t)
	id := mkRunning(t, e, s, "/wt/o", "base")
	seedTerminalWithAgent(t, s, id, "wZ:p1", "term_A")

	// a DIFFERENT process now holds the same pane_id (herdr recycled it)
	fake.Agents = []core.AgentObs{{Ref: "wZ:p1", Workspace: "someone-else", BootID: "term_B", Alive: true}}
	res, err := e.Sweep(context.Background())
	require.NoError(t, err)
	require.Equal(t, 0, res.AgentsReaped, "terminal_id mismatch → not our agent → do not close")
	require.Empty(t, fake.Killed(), "must never close a workspace we can't positively identify")

	// identity unknown (herdr reported no terminal_id) → also skip, never guess
	fake.Agents = []core.AgentObs{{Ref: "wZ:p1", Workspace: "arco_" + id, BootID: "", Alive: true}}
	res, err = e.Sweep(context.Background())
	require.NoError(t, err)
	require.Equal(t, 0, res.AgentsReaped, "unknown identity → skip, don't guess")
	require.Empty(t, fake.Killed())
}
