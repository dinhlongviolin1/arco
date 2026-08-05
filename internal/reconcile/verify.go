package reconcile

import (
	"context"
	"fmt"

	"github.com/dinhlongviolin1/arco/internal/core"
)

// WorkerDiff returns the base→head diff for a worker (via the VMClient). It is a
// read; it does not change state.
func (e *Engine) WorkerDiff(ctx context.Context, workerID string) (core.Diff, error) {
	w, err := e.Store.Reader().GetWorker(workerID)
	if err != nil {
		return core.Diff{}, err
	}
	return e.VM.Diff(ctx, w.Worktree, w.BaseCommit, w.HeadCommit)
}

// Verify is the diff-gate: it moves a worker completed_candidate →
// completed_verified after its base→head diff has been reviewed (by a human or
// an auto-verifier). It refuses any worker not in completed_candidate — a worker
// never reaches verified on a guess (build-guide Task 16).
func (e *Engine) Verify(ctx context.Context, workerID string) error {
	return e.Store.WithTx(ctx, func(tx core.Tx) error {
		w, err := tx.GetWorker(workerID)
		if err != nil {
			return err
		}
		if w.State != core.WorkerCompletedCandidate {
			return fmt.Errorf("%w: verify requires completed_candidate, got %s", core.ErrIllegalTransition, w.State)
		}
		return tx.TransitionWorker(workerID, core.WorkerCompletedVerified, w.Rev, core.Event{
			Kind: "state_change", WorkerID: workerID, SessionID: w.OwnerSession,
			Payload: `{"verified":true}`,
		})
	})
}
