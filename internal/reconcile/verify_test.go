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
	err = e.Verify(context.Background(), res.WorkerID, 1, "human")
	require.ErrorIs(t, err, core.ErrIllegalTransition)

	// drive to completed_candidate (idle + HEAD advanced)
	require.NoError(t, e.ApplyEvent(context.Background(), EventInput{
		WorkerID: res.WorkerID, HerdrState: "idle", Alive: true, ObservedHead: "abc1234",
	}))
	w, _ := s.Reader().GetWorker(res.WorkerID)
	require.Equal(t, core.WorkerCompletedCandidate, w.State)

	// now verify succeeds with the observed rev
	require.NoError(t, e.Verify(context.Background(), res.WorkerID, w.Rev, "human"))
	w, _ = s.Reader().GetWorker(res.WorkerID)
	require.Equal(t, core.WorkerCompletedVerified, w.State)

	// double-verify is rejected (already verified, terminal)
	require.ErrorIs(t, e.Verify(context.Background(), res.WorkerID, w.Rev, "human"), core.ErrIllegalTransition)
	_ = fake
}

// Regression (opus/qwen P1): a candidate that re-ran to a new HEAD between diff
// review and verify must be refused when verified against the OLD rev.
func TestVerify_StaleRevRefused_NoVerifyOnAGuess(t *testing.T) {
	e, s, _ := newEngine(t)
	res, err := e.Dispatch(context.Background(), "", "task", true)
	require.NoError(t, err)

	// candidate at H1
	require.NoError(t, e.ApplyEvent(context.Background(), EventInput{WorkerID: res.WorkerID, HerdrState: "idle", Alive: true, ObservedHead: "1111111"}))
	reviewed, _ := s.Reader().GetWorker(res.WorkerID)
	require.Equal(t, core.WorkerCompletedCandidate, reviewed.State)
	oldRev := reviewed.Rev // the rev the "human" reviewed

	// it re-runs and returns to candidate at a NEW head H2 (rev advances)
	require.NoError(t, e.ApplyEvent(context.Background(), EventInput{WorkerID: res.WorkerID, HerdrState: "working", Alive: true}))
	require.NoError(t, e.ApplyEvent(context.Background(), EventInput{WorkerID: res.WorkerID, HerdrState: "idle", Alive: true, ObservedHead: "2222222"}))
	now, _ := s.Reader().GetWorker(res.WorkerID)
	require.Equal(t, core.WorkerCompletedCandidate, now.State)
	require.NotEqual(t, oldRev, now.Rev)

	// verifying against the stale rev must be refused (never bless an unreviewed diff)
	require.ErrorIs(t, e.Verify(context.Background(), res.WorkerID, oldRev, "human"), core.ErrRevMismatch)
	w, _ := s.Reader().GetWorker(res.WorkerID)
	require.Equal(t, core.WorkerCompletedCandidate, w.State, "still unverified")

	// verifying against the CURRENT rev succeeds
	require.NoError(t, e.Verify(context.Background(), res.WorkerID, now.Rev, "human"))
	w, _ = s.Reader().GetWorker(res.WorkerID)
	require.Equal(t, core.WorkerCompletedVerified, w.State)
}

func TestWorkerDiff_CallsVMClient(t *testing.T) {
	e, _, _ := newEngine(t)
	res, err := e.Dispatch(context.Background(), "", "task", true)
	require.NoError(t, err)
	d, err := e.WorkerDiff(context.Background(), res.WorkerID)
	require.NoError(t, err) // fake returns an empty diff, no error
	require.Equal(t, 0, d.Files)
}
