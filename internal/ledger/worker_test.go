package ledger

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
)

func TestWorker_CreateGetList(t *testing.T) {
	s := newTestStore(t)
	sess := newWork(t, s)
	id := newWorker(t, s, sess)

	w, err := s.Reader().GetWorker(id)
	require.NoError(t, err)
	require.Equal(t, core.WorkerStarting, w.State)
	require.Equal(t, sess, w.OwnerSession)
	require.Equal(t, "arco_"+id, w.Workspace)

	list, err := s.Reader().ListWorkers(core.WorkerFilter{OwnerSession: sess})
	require.NoError(t, err)
	require.Len(t, list, 1)

	_, err = s.Reader().GetWorker("nope")
	require.ErrorIs(t, err, core.ErrNotFound)
}

func TestWorker_GuardedTransition_LegalAndEvent(t *testing.T) {
	s := newTestStore(t)
	id := newWorker(t, s, newWork(t, s))

	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.TransitionWorker(id, core.WorkerRunning, 0, core.Event{Kind: "state_change", WorkerID: id})
	}))
	w, _ := s.Reader().GetWorker(id)
	require.Equal(t, core.WorkerRunning, w.State)
	require.Equal(t, int64(1), w.Rev)

	// a state_change event was appended for this worker.
	var n int
	require.NoError(t, s.DB().QueryRow(`SELECT COUNT(1) FROM events WHERE worker_id=? AND kind='state_change'`, id).Scan(&n))
	require.Equal(t, 1, n)
}

func TestWorker_IllegalTransitionRejected(t *testing.T) {
	s := newTestStore(t)
	id := newWorker(t, s, newWork(t, s))
	// drive to killed (terminal)
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.TransitionWorker(id, core.WorkerKilled, 0, core.Event{Kind: "kill_done", WorkerID: id})
	}))
	// killed -> running is illegal
	err := s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.TransitionWorker(id, core.WorkerRunning, 1, core.Event{Kind: "state_change", WorkerID: id})
	})
	require.ErrorIs(t, err, core.ErrIllegalTransition)
}

func TestWorker_CASMismatchOnStaleRev(t *testing.T) {
	s := newTestStore(t)
	id := newWorker(t, s, newWork(t, s))
	// advance rev 0 -> 1
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.TransitionWorker(id, core.WorkerRunning, 0, core.Event{Kind: "state_change", WorkerID: id})
	}))
	// stale expectedRev=0 must fail the CAS (no side effect without a CAS win)
	err := s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.TransitionWorker(id, core.WorkerBlocked, 0, core.Event{Kind: "state_change", WorkerID: id})
	})
	require.ErrorIs(t, err, core.ErrRevMismatch)
	w, _ := s.Reader().GetWorker(id)
	require.Equal(t, core.WorkerRunning, w.State) // unchanged
}

func TestWorker_CreateRequiresOwnerSession(t *testing.T) {
	s := newTestStore(t)
	err := s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.CreateWorker(core.Worker{ID: "x", State: core.WorkerStarting})
	})
	require.Error(t, err)
	require.False(t, errors.Is(err, core.ErrRevMismatch))
}
