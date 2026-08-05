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
// completed_verified after its base→head diff has been reviewed. The caller MUST
// pass the rev it observed when reviewing the diff (from GET /diff); the CAS is
// against THAT rev, not an in-tx re-read — so if the candidate re-ran to a new
// HEAD between review and verify (candidate→running→candidate'), the rev no
// longer matches and verify is refused (ErrRevMismatch). This is what makes
// "never verify on a guess" real (build-guide Task 16).
func (e *Engine) Verify(ctx context.Context, workerID string, expectedRev int64, actor string) error {
	if actor == "" {
		actor = "human"
	}
	return e.Store.WithTx(ctx, func(tx core.Tx) error {
		w, err := tx.GetWorker(workerID)
		if err != nil {
			return err
		}
		if w.State != core.WorkerCompletedCandidate {
			return fmt.Errorf("%w: verify requires completed_candidate, got %s", core.ErrIllegalTransition, w.State)
		}
		if w.HeadCommit == "" {
			return fmt.Errorf("%w: nothing to verify (no head commit)", core.ErrIllegalTransition)
		}
		// Record exactly what was verified (base→head), attributed to the actor,
		// so the ledger proves WHAT was blessed and BY WHOM.
		return tx.TransitionWorker(workerID, core.WorkerCompletedVerified, expectedRev, core.Event{
			Kind: "state_change", WorkerID: workerID, SessionID: w.OwnerSession, Actor: actor,
			Payload: fmt.Sprintf(`{"verified":true,"base":%q,"head":%q}`, w.BaseCommit, w.HeadCommit),
		})
	})
}
