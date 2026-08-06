package reconcile

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
)

func TestDelegate_SpawnsChildWithLineage(t *testing.T) {
	e, s, fake := newEngine(t)
	parent := dispatchRunning(t, e)
	p, _ := s.Reader().GetWorker(parent)

	res, err := e.Delegate(context.Background(), parent, "do the subtask", "")
	require.NoError(t, err)
	require.Equal(t, core.WorkerRunning, res.State)
	require.Equal(t, p.OwnerSession, res.SessionID, "child inherits the parent's session")

	child, err := s.Reader().GetWorker(res.WorkerID)
	require.NoError(t, err)
	require.Equal(t, parent, child.ParentWorkerID)
	require.Equal(t, 1, child.DelegationDepth)
	require.Equal(t, core.WorkerRunning, child.State)

	// the child agent was actually launched with its task
	var launched bool
	for _, pr := range fake.Prompts() {
		if pr.Workspace == child.Workspace && pr.Text == "do the subtask" {
			launched = true
		}
	}
	require.True(t, launched, "child worker launched via VMClient")
}

func TestDelegate_DepthCapAtTwo(t *testing.T) {
	e, s, _ := newEngine(t)
	parent := dispatchRunning(t, e) // depth 0
	c1, err := e.Delegate(context.Background(), parent, "level 1", "")
	require.NoError(t, err)
	require.Equal(t, 1, mustWorker(t, s, c1.WorkerID).DelegationDepth)

	c2, err := e.Delegate(context.Background(), c1.WorkerID, "level 2", "")
	require.NoError(t, err)
	require.Equal(t, 2, mustWorker(t, s, c2.WorkerID).DelegationDepth)

	// a depth-2 worker cannot delegate further (would be depth 3)
	_, err = e.Delegate(context.Background(), c2.WorkerID, "level 3", "")
	require.ErrorIs(t, err, core.ErrMaxDepthExceeded)
}

func TestDelegate_FanInCap(t *testing.T) {
	e, s, _ := newEngine(t)
	e.MaxChildren = 3 // the session may hold at most 3 active workers
	parent := dispatchRunning(t, e)
	// parent is worker #1 in the session; two children fill the cap.
	_, err := e.Delegate(context.Background(), parent, "c1", "")
	require.NoError(t, err)
	_, err = e.Delegate(context.Background(), parent, "c2", "")
	require.NoError(t, err)
	// 4th active worker would exceed the cap
	_, err = e.Delegate(context.Background(), parent, "c3", "")
	require.ErrorIs(t, err, core.ErrFanInExceeded)

	n, err := s.Reader().CountActiveWorkers(mustWorker(t, s, parent).OwnerSession)
	require.NoError(t, err)
	require.Equal(t, 3, n, "cap holds; no over-admission")
}

// A finished (terminal) child frees a fan-in slot for a new delegation.
func TestDelegate_TerminalChildFreesSlot(t *testing.T) {
	e, s, _ := newEngine(t)
	e.MaxChildren = 2
	parent := dispatchRunning(t, e)
	c1, err := e.Delegate(context.Background(), parent, "c1", "")
	require.NoError(t, err)
	require.ErrorIs(t, e.delegateExpectFanIn(t, parent), core.ErrFanInExceeded)

	// drive c1 terminal → frees its slot
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		w, _ := tx.GetWorker(c1.WorkerID)
		return tx.TransitionWorker(c1.WorkerID, core.WorkerFailed, w.Rev, core.Event{Kind: "state_change", WorkerID: c1.WorkerID, SessionID: w.OwnerSession, Payload: "{}"})
	}))
	_, err = e.Delegate(context.Background(), parent, "c2", "")
	require.NoError(t, err, "a terminal worker no longer counts against the cap")
}

func TestDelegate_TerminalParentRejected(t *testing.T) {
	e, s, _ := newEngine(t)
	parent := dispatchRunning(t, e)
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		w, _ := tx.GetWorker(parent)
		return tx.TransitionWorker(parent, core.WorkerFailed, w.Rev, core.Event{Kind: "state_change", WorkerID: parent, SessionID: w.OwnerSession, Payload: "{}"})
	}))
	_, err := e.Delegate(context.Background(), parent, "sub", "")
	require.ErrorIs(t, err, core.ErrIllegalTransition)
}

// The brain's `dispatch` StepResult spawns a child (not a re-prompt of itself).
func TestBrain_DispatchDelegatesChild(t *testing.T) {
	e, s, _ := brainEngine(t, `{"kind":"dispatch","instruction":"handle the sub-part"}`, nil)
	parent := dispatchRunning(t, e)
	pSession := mustWorker(t, s, parent).OwnerSession

	require.NoError(t, e.ApplyEvent(context.Background(), ambiguousEvent(parent)))
	e.Exec.Wait()

	// parent stays running; a new child worker now exists in the session
	require.Equal(t, core.WorkerRunning, mustWorker(t, s, parent).State)
	ws, err := s.Reader().ListWorkers(core.WorkerFilter{OwnerSession: pSession})
	require.NoError(t, err)
	var child *core.Worker
	for i := range ws {
		if ws[i].ParentWorkerID == parent {
			child = &ws[i]
		}
	}
	require.NotNil(t, child, "dispatch spawned a child worker")
	require.Equal(t, 1, child.DelegationDepth)
}

// A denied delegation (fan-in) from the brain is an audit event, not a crash.
func TestBrain_DispatchDeniedRecordsError(t *testing.T) {
	e, s, _ := brainEngine(t, `{"kind":"dispatch","instruction":"sub"}`, nil)
	e.MaxChildren = 1 // parent alone already fills the session
	parent := dispatchRunning(t, e)

	require.NoError(t, e.ApplyEvent(context.Background(), ambiguousEvent(parent)))
	e.Exec.Wait()

	require.Equal(t, core.WorkerRunning, mustWorker(t, s, parent).State, "parent survives a denied delegation")
	evs, _ := s.Reader().EventsSince(0, 100000)
	found := false
	for _, ev := range evs {
		if ev.Kind == "error" && ev.WorkerID == parent {
			found = true
		}
	}
	require.True(t, found, "denied delegation recorded an error event")
}

func mustWorker(t *testing.T, s interface {
	Reader() core.Reader
}, id string) core.Worker {
	t.Helper()
	w, err := s.Reader().GetWorker(id)
	require.NoError(t, err)
	return w
}

// delegateExpectFanIn is a tiny helper returning the error of a delegation that
// should hit the fan-in cap (keeps the table test terse).
func (e *Engine) delegateExpectFanIn(t *testing.T, parent string) error {
	t.Helper()
	_, err := e.Delegate(context.Background(), parent, "overflow", "")
	return err
}
