package reconcile

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/ledger"
	"github.com/dinhlongviolin1/arco/internal/vm"
)

// brainEngine wires an engine whose brain returns a canned output.
func brainEngine(t *testing.T, out string, runErr error) (*Engine, *ledger.Store, *vm.Fake) {
	e, s, fake := newEngine(t)
	e.Brain = BrainCfg{Enabled: true, Profile: "p", Model: "m",
		Runner: func(context.Context, string, ...string) ([]byte, error) { return []byte(out), runErr }}
	return e, s, fake
}

// ambiguousEvent produces a fusion-ambiguous signal (unknown herdr state, alive).
func ambiguousEvent(id string) EventInput {
	return EventInput{WorkerID: id, HerdrState: "", Alive: true}
}

func dispatchRunning(t *testing.T, e *Engine) string {
	t.Helper()
	res, err := e.Dispatch(context.Background(), "", "task", true)
	require.NoError(t, err)
	return res.WorkerID
}

func TestBrain_FinalOutput_Completes(t *testing.T) {
	e, s, _ := brainEngine(t, `{"kind":"final_output","reason":"done"}`, nil)
	id := dispatchRunning(t, e)
	require.NoError(t, e.ApplyEvent(context.Background(), ambiguousEvent(id)))
	e.Exec.Wait()
	w, _ := s.Reader().GetWorker(id)
	require.Equal(t, core.WorkerCompletedCandidate, w.State)
}

func TestBrain_Question_OpensEscalation(t *testing.T) {
	e, s, _ := brainEngine(t, `{"kind":"question","instruction":"which framework?"}`, nil)
	id := dispatchRunning(t, e)
	require.NoError(t, e.ApplyEvent(context.Background(), ambiguousEvent(id)))
	e.Exec.Wait()
	w, _ := s.Reader().GetWorker(id)
	require.Equal(t, core.WorkerWaitingForUser, w.State)
	esc, _ := s.Reader().ListEscalations(core.EscalationFilter{Status: "pending", WorkerID: id})
	require.Len(t, esc, 1)
	require.Equal(t, "question", esc[0].Kind)
}

func TestBrain_RunAgain_PromptsWorker(t *testing.T) {
	e, s, fake := brainEngine(t, `{"kind":"run_again","instruction":"keep going"}`, nil)
	id := dispatchRunning(t, e)
	require.NoError(t, e.ApplyEvent(context.Background(), ambiguousEvent(id)))
	e.Exec.Wait()
	w, _ := s.Reader().GetWorker(id)
	require.Equal(t, core.WorkerRunning, w.State)
	prompts := fake.Prompts()
	require.NotEmpty(t, prompts)
	last := prompts[len(prompts)-1]
	require.Contains(t, last.Text, "keep going")
	require.Contains(t, last.Text, "[arco-intent]")
}

func TestBrain_MalformedParksBlocked(t *testing.T) {
	e, s, _ := brainEngine(t, `not json at all`, nil)
	id := dispatchRunning(t, e)
	require.NoError(t, e.ApplyEvent(context.Background(), ambiguousEvent(id)))
	e.Exec.Wait()
	w, _ := s.Reader().GetWorker(id)
	require.Equal(t, core.WorkerBlocked, w.State)
}

func TestBrain_BillingParksBlockedNotRetried(t *testing.T) {
	e, s, _ := brainEngine(t, `Error: insufficient balance`, errors.New("exit 1"))
	id := dispatchRunning(t, e)
	require.NoError(t, e.ApplyEvent(context.Background(), ambiguousEvent(id)))
	e.Exec.Wait()
	w, _ := s.Reader().GetWorker(id)
	require.Equal(t, core.WorkerBlocked, w.State)
}

// Regression (opus P2): a worker parked `blocked` by a billing wall must NOT be
// re-classified by the brain on the next ambiguous signal (no clavis storm).
func TestBrain_BlockedNotReclassified(t *testing.T) {
	var calls int32
	e, s, _ := newEngine(t)
	e.Brain = BrainCfg{Enabled: true, Profile: "p", Model: "m",
		Runner: func(context.Context, string, ...string) ([]byte, error) {
			atomic.AddInt32(&calls, 1)
			return []byte("insufficient balance"), errors.New("exit 1")
		}}
	id := dispatchRunning(t, e)

	require.NoError(t, e.ApplyEvent(context.Background(), ambiguousEvent(id)))
	e.Exec.Wait()
	w, _ := s.Reader().GetWorker(id)
	require.Equal(t, core.WorkerBlocked, w.State)
	require.Equal(t, int32(1), atomic.LoadInt32(&calls))

	// another ambiguous signal on the parked worker must not call the brain again
	require.NoError(t, e.ApplyEvent(context.Background(), ambiguousEvent(id)))
	e.Exec.Wait()
	require.Equal(t, int32(1), atomic.LoadInt32(&calls), "parked worker must not be re-classified")
}

func TestBrain_DisabledLeavesWorkerUnchanged(t *testing.T) {
	e, s, _ := newEngine(t) // brain disabled by default
	id := dispatchRunning(t, e)
	require.NoError(t, e.ApplyEvent(context.Background(), ambiguousEvent(id)))
	e.Exec.Wait()
	w, _ := s.Reader().GetWorker(id)
	require.Equal(t, core.WorkerRunning, w.State, "no brain call when disabled")
}
