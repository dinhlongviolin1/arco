package reconcile

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
)

func TestVerify_OnlyFromCompletedCandidate(t *testing.T) {
	e, s, fake := newEngine(t)
	e.MissThreshold = 1
	res, err := e.Dispatch(context.Background(), "", "task", true)
	require.NoError(t, err)

	// verify while still running → rejected (no verify on a guess)
	err = e.Verify(context.Background(), res.WorkerID)
	require.ErrorIs(t, err, core.ErrIllegalTransition)

	// drive to completed_candidate (idle + HEAD advanced)
	require.NoError(t, e.ApplyEvent(context.Background(), EventInput{
		WorkerID: res.WorkerID, HerdrState: "idle", Alive: true, ObservedHead: "abc",
	}))
	w, _ := s.Reader().GetWorker(res.WorkerID)
	require.Equal(t, core.WorkerCompletedCandidate, w.State)

	// now verify succeeds
	require.NoError(t, e.Verify(context.Background(), res.WorkerID))
	w, _ = s.Reader().GetWorker(res.WorkerID)
	require.Equal(t, core.WorkerCompletedVerified, w.State)

	// double-verify is rejected (already verified, terminal)
	require.ErrorIs(t, e.Verify(context.Background(), res.WorkerID), core.ErrIllegalTransition)
	_ = fake
}

func TestWorkerDiff_CallsVMClient(t *testing.T) {
	e, _, _ := newEngine(t)
	res, err := e.Dispatch(context.Background(), "", "task", true)
	require.NoError(t, err)
	d, err := e.WorkerDiff(context.Background(), res.WorkerID)
	require.NoError(t, err) // fake returns an empty diff, no error
	require.Equal(t, 0, d.Files)
}
