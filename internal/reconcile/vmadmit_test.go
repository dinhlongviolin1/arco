package reconcile

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
)

// With a VM assigned and a per-VM cap, dispatch is admitted up to the cap and
// rejected past it — enforced atomically in the create tx (single-writer),
// mirroring the fan-in/lease pattern.
func TestDispatch_PerVMCap(t *testing.T) {
	e, s, _ := newEngine(t)
	e.DefaultVM = "local"
	e.MaxWorkersPerVM = 2

	_, err := e.Dispatch(context.Background(), "", "w1", true)
	require.NoError(t, err)
	_, err = e.Dispatch(context.Background(), "", "w2", true)
	require.NoError(t, err)
	// third exceeds the VM cap
	_, err = e.Dispatch(context.Background(), "", "w3", true)
	require.ErrorIs(t, err, core.ErrVMAtCapacity)

	n, err := s.Reader().CountActiveWorkersOnVM("local")
	require.NoError(t, err)
	require.Equal(t, 2, n, "cap holds; no over-admission")
}

// A terminal worker frees its VM slot.
func TestDispatch_PerVMCapTerminalFreesSlot(t *testing.T) {
	e, s, _ := newEngine(t)
	e.DefaultVM = "local"
	e.MaxWorkersPerVM = 1

	r1, err := e.Dispatch(context.Background(), "", "w1", true)
	require.NoError(t, err)
	_, err = e.Dispatch(context.Background(), "", "w2", true)
	require.ErrorIs(t, err, core.ErrVMAtCapacity)

	// drive w1 terminal → frees the VM slot
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		w, _ := tx.GetWorker(r1.WorkerID)
		return tx.TransitionWorker(r1.WorkerID, core.WorkerFailed, w.Rev, core.Event{Kind: "state_change", WorkerID: r1.WorkerID, SessionID: w.OwnerSession, Payload: "{}"})
	}))
	_, err = e.Dispatch(context.Background(), "", "w2", true)
	require.NoError(t, err, "a terminal worker no longer counts against the VM cap")
}

// Inert by default: with no DefaultVM (or no cap) the check never fires, so
// existing single-VM-less behavior is unchanged.
func TestDispatch_PerVMCapInertWhenUnset(t *testing.T) {
	e, s, _ := newEngine(t) // DefaultVM "" , MaxWorkersPerVM 0
	for i := 0; i < 5; i++ {
		_, err := e.Dispatch(context.Background(), "", "w", true)
		require.NoError(t, err)
	}
	// workers have no VM assigned → the per-VM counter for "" is all of them, but
	// the cap is disabled so nothing is rejected.
	require.Equal(t, core.WorkerState(core.WorkerRunning), mustWorker(t, s, dispatchRunning(t, e)).State)
}

// Delegated children also respect the per-VM cap.
func TestDelegate_PerVMCap(t *testing.T) {
	e, _, _ := newEngine(t)
	e.DefaultVM = "local"
	e.MaxWorkersPerVM = 2 // parent fills 1, one child fits, second child rejected

	parent := dispatchRunning(t, e)
	_, err := e.Delegate(context.Background(), parent, "c1")
	require.NoError(t, err)
	_, err = e.Delegate(context.Background(), parent, "c2")
	require.ErrorIs(t, err, core.ErrVMAtCapacity)
}
