package reconcile

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/ledger"
)

func completeChild(t *testing.T, s *ledger.Store, childID string) {
	t.Helper()
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		w, _ := tx.GetWorker(childID)
		return tx.TransitionWorker(childID, core.WorkerFailed, w.Rev, core.Event{Kind: "state_change", WorkerID: childID, SessionID: w.OwnerSession, Payload: "{}"})
	}))
}

func TestRollup_FiresOnChildCompletionWithContext(t *testing.T) {
	var calls atomic.Int32
	var prompt string
	e, s, _ := newEngine(t)
	e.RollupInterval = time.Hour
	e.Brain = BrainCfg{Enabled: true, Profile: "p", Model: "m",
		Runner: func(_ context.Context, _ string, args ...string) ([]byte, error) {
			calls.Add(1)
			prompt = strings.Join(args, " ")
			return []byte(`{"kind":"run_again","instruction":"keep steering"}`), nil
		}}

	parent := dispatchRunning(t, e)
	child, err := e.Delegate(context.Background(), parent, "subtask alpha")
	require.NoError(t, err)
	completeChild(t, s, child.WorkerID)

	res, err := e.Sweep(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, res.RollupsTriggered)
	e.Exec.Wait()

	require.Equal(t, int32(1), calls.Load(), "rollup brain call fired once")
	require.Contains(t, prompt, "Completed sub-workers")
	require.Contains(t, prompt, "subtask alpha", "child result is in the rollup context")

	// a tainted rollup_intent (M19 provenance) was recorded for the parent
	evs, _ := s.Reader().EventsSince(0, 100000)
	var ri *core.Event
	for i := range evs {
		if evs[i].Kind == "rollup_intent" && evs[i].WorkerID == parent {
			ri = &evs[i]
		}
	}
	require.NotNil(t, ri, "rollup_intent recorded")
	require.Contains(t, ri.Payload, `"call_kind":"rollup"`)
	require.Contains(t, ri.Payload, `"tainted":true`)
}

func TestRollup_CoalescesWithinInterval(t *testing.T) {
	var calls atomic.Int32
	var prompt string
	e, s, _ := newEngine(t)
	e.RollupInterval = time.Hour
	e.Brain = BrainCfg{Enabled: true, Profile: "p", Model: "m",
		Runner: func(_ context.Context, _ string, args ...string) ([]byte, error) {
			calls.Add(1)
			prompt = strings.Join(args, " ")
			return []byte(`{"kind":"run_again","instruction":"go"}`), nil
		}}

	parent := dispatchRunning(t, e)
	c1, _ := e.Delegate(context.Background(), parent, "c1")
	c2, _ := e.Delegate(context.Background(), parent, "c2")
	completeChild(t, s, c1.WorkerID)
	completeChild(t, s, c2.WorkerID)

	_, err := e.Sweep(context.Background())
	require.NoError(t, err)
	e.Exec.Wait()
	_, err = e.Sweep(context.Background()) // same interval → must coalesce
	require.NoError(t, err)
	e.Exec.Wait()

	require.Equal(t, int32(1), calls.Load(), "≤1 rollup brain call per interval despite two sweeps + two children")
	_ = prompt
}

func TestRollup_DisabledByZeroInterval(t *testing.T) {
	var calls atomic.Int32
	e, s, _ := newEngine(t)
	e.RollupInterval = 0 // disabled
	e.Brain = BrainCfg{Enabled: true, Profile: "p", Model: "m",
		Runner: func(_ context.Context, _ string, _ ...string) ([]byte, error) {
			calls.Add(1)
			return []byte(`{"kind":"run_again"}`), nil
		}}
	parent := dispatchRunning(t, e)
	child, _ := e.Delegate(context.Background(), parent, "c")
	completeChild(t, s, child.WorkerID)

	res, err := e.Sweep(context.Background())
	require.NoError(t, err)
	e.Exec.Wait()
	require.Equal(t, 0, res.RollupsTriggered)
	require.Equal(t, int32(0), calls.Load())
}

func TestRollup_SkippedWhenParentTerminal(t *testing.T) {
	var calls atomic.Int32
	e, s, _ := newEngine(t)
	e.RollupInterval = time.Hour
	e.Brain = BrainCfg{Enabled: true, Profile: "p", Model: "m",
		Runner: func(_ context.Context, _ string, _ ...string) ([]byte, error) {
			calls.Add(1)
			return []byte(`{"kind":"run_again"}`), nil
		}}
	parent := dispatchRunning(t, e)
	child, _ := e.Delegate(context.Background(), parent, "c")
	completeChild(t, s, child.WorkerID)
	// parent itself goes terminal → nothing to steer
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		w, _ := tx.GetWorker(parent)
		return tx.TransitionWorker(parent, core.WorkerFailed, w.Rev, core.Event{Kind: "state_change", WorkerID: parent, SessionID: w.OwnerSession, Payload: "{}"})
	}))

	res, err := e.Sweep(context.Background())
	require.NoError(t, err)
	e.Exec.Wait()
	require.Equal(t, 0, res.RollupsTriggered, "a terminal parent is not rolled up")
	require.Equal(t, int32(0), calls.Load())
}
