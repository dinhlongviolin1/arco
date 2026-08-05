package reconcile

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/ledger"
	"github.com/dinhlongviolin1/arco/internal/vm"
)

func newEngine(t *testing.T) (*Engine, *ledger.Store, *vm.Fake) {
	t.Helper()
	s, err := ledger.Open(filepath.Join(t.TempDir(), "e.db"))
	require.NoError(t, err)
	require.NoError(t, s.Migrate(context.Background()))
	t.Cleanup(func() { s.Close() })
	fake := vm.NewFake()
	return New(s, fake), s, fake
}

func TestDispatch_NewSession_CrashSafeIntentThenRunning(t *testing.T) {
	e, s, fake := newEngine(t)
	res, err := e.Dispatch(context.Background(), "", "do the thing", true)
	require.NoError(t, err)
	require.NotEmpty(t, res.SessionID)
	require.NotEmpty(t, res.WorkerID)

	w, err := s.Reader().GetWorker(res.WorkerID)
	require.NoError(t, err)
	require.Equal(t, core.WorkerRunning, w.State)
	require.Equal(t, "arco_"+res.WorkerID, w.Workspace)

	// the agent was launched with the task
	require.Len(t, fake.Prompts(), 1)
	require.Equal(t, "do the thing", fake.Prompts()[0].Text)

	// crash-safe ordering: dispatch_intent precedes dispatch_done
	evs, _ := s.Reader().EventsSince(0, 100)
	var iIntent, iDone = -1, -1
	for i, ev := range evs {
		switch ev.Kind {
		case "dispatch_intent":
			iIntent = i
		case "dispatch_done":
			iDone = i
		}
	}
	require.GreaterOrEqual(t, iIntent, 0)
	require.GreaterOrEqual(t, iDone, 0)
	require.Less(t, iIntent, iDone, "dispatch_intent must precede dispatch_done")

	// session is active
	sess, _ := s.Reader().GetSession(res.SessionID)
	require.Equal(t, core.SessionActive, sess.Status)
}

func TestDispatch_LaunchErrorParksFailed(t *testing.T) {
	e, s, fake := newEngine(t)
	fake.PromptErr = errors.New("clavis boom")
	res, err := e.Dispatch(context.Background(), "", "task", true)
	require.NoError(t, err) // dispatch itself succeeds; the worker is parked failed
	require.Equal(t, core.WorkerFailed, res.State, "DispatchResult.State must report the launch failure")
	w, _ := s.Reader().GetWorker(res.WorkerID)
	require.Equal(t, core.WorkerFailed, w.State)
}

// Regression (qwen finding #2): an ambiguous launch error where the agent is
// actually alive must be adopted running, not failed over a live process.
func TestDispatch_AmbiguousLaunchErrorButAlive_AdoptsRunning(t *testing.T) {
	e, s, fake := newEngine(t)
	fake.AliveOnPrompt = true
	fake.PromptErr = errors.New("connection dropped after spawn")
	res, err := e.Dispatch(context.Background(), "", "task", true)
	require.NoError(t, err)
	require.Equal(t, core.WorkerRunning, res.State, "a live agent must be adopted, not failed")
	w, _ := s.Reader().GetWorker(res.WorkerID)
	require.Equal(t, core.WorkerRunning, w.State)
}

func TestApplyEvent_IdleWithHeadChange_Completes(t *testing.T) {
	e, s, _ := newEngine(t)
	res, err := e.Dispatch(context.Background(), "", "task", true)
	require.NoError(t, err)

	// herdr says idle and HEAD advanced → completed_candidate
	require.NoError(t, e.ApplyEvent(context.Background(), EventInput{
		WorkerID: res.WorkerID, HerdrState: "idle", Alive: true, ObservedHead: "deadbeef",
	}))
	w, _ := s.Reader().GetWorker(res.WorkerID)
	require.Equal(t, core.WorkerCompletedCandidate, w.State)
	require.Equal(t, "deadbeef", w.HeadCommit)
}

func TestApplyEvent_WaitingInputOpensEscalation(t *testing.T) {
	e, s, _ := newEngine(t)
	res, err := e.Dispatch(context.Background(), "", "task", true)
	require.NoError(t, err)
	require.NoError(t, e.ApplyEvent(context.Background(), EventInput{
		WorkerID: res.WorkerID, HerdrState: "idle", Alive: true, WaitingInput: true,
	}))
	w, _ := s.Reader().GetWorker(res.WorkerID)
	require.Equal(t, core.WorkerWaitingForUser, w.State)

	pending, err := s.Reader().ListEscalations(core.EscalationFilter{Status: "pending", WorkerID: res.WorkerID})
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, "question", pending[0].Kind)

	// a second waiting event must not pile up a second escalation (one-pending)
	require.NoError(t, e.ApplyEvent(context.Background(), EventInput{
		WorkerID: res.WorkerID, HerdrState: "idle", Alive: true, WaitingInput: true,
	}))
	pending, _ = s.Reader().ListEscalations(core.EscalationFilter{Status: "pending", WorkerID: res.WorkerID})
	require.Len(t, pending, 1)
}

// Regression (opus P2): a worker that leaves waiting via a later herdr signal
// must have its phantom pending escalation expired.
func TestApplyEvent_LeavingWaitingExpiresEscalation(t *testing.T) {
	e, s, _ := newEngine(t)
	res, err := e.Dispatch(context.Background(), "", "task", true)
	require.NoError(t, err)
	// go waiting_for_user → opens an escalation
	require.NoError(t, e.ApplyEvent(context.Background(), EventInput{WorkerID: res.WorkerID, HerdrState: "idle", Alive: true, WaitingInput: true}))
	pending, _ := s.Reader().ListEscalations(core.EscalationFilter{Status: "pending", WorkerID: res.WorkerID})
	require.Len(t, pending, 1)

	// a later herdr signal drives it back to running (worker un-stuck by itself)
	require.NoError(t, e.ApplyEvent(context.Background(), EventInput{WorkerID: res.WorkerID, HerdrState: "working", Alive: true}))
	w, _ := s.Reader().GetWorker(res.WorkerID)
	require.Equal(t, core.WorkerRunning, w.State)
	pending, _ = s.Reader().ListEscalations(core.EscalationFilter{Status: "pending", WorkerID: res.WorkerID})
	require.Empty(t, pending, "leaving the waiting state must expire the phantom escalation")
}

func TestApplyEvent_UnknownStateNoChange(t *testing.T) {
	e, s, _ := newEngine(t)
	res, _ := e.Dispatch(context.Background(), "", "task", true)
	require.NoError(t, e.ApplyEvent(context.Background(), EventInput{WorkerID: res.WorkerID, HerdrState: "", Alive: true}))
	w, _ := s.Reader().GetWorker(res.WorkerID)
	require.Equal(t, core.WorkerRunning, w.State) // unchanged
}
