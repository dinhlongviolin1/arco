package reconcile

import (
	"context"
	"fmt"

	"github.com/dinhlongviolin1/arco/internal/core"
)

// RedeliverInitialTask re-prompts a worker with its original task, on explicit
// OPERATOR request — the recovery path for a worker stranded taskless by a crash
// between the running commit and the initial-task delivery (whole-system audit
// MED-3). It is deliberately NOT autonomous: a finished-but-idle agent is
// indistinguishable from a never-delivered one, so unattended re-delivery can
// double-execute a completed task (the double-dispatch class the review rejects).
// The operator inspects the pane first; this adds the guards that ARE
// machine-checkable:
//   - the worker must be running and not pool-owned (re-validated under the write
//     lock at the point of no return, so a concurrent handoff-to-pool between the
//     initial read and the prompt can't be prompted past — mirrors preparePrompt);
//   - refuse an agent herdr reports working/blocked (interrupting it injects a
//     duplicate task);
//   - refuse when the worker's HEAD already advanced past its base commit — the
//     agent committed work, so it almost certainly received the task and a
//     re-prompt would double-execute (ledger-only; catches every commit-producing
//     task in the idle-residual).
//
// The remaining fast-finish, no-commit residual is the operator's judgment call.
// Success records prompt_delivered (resolving the dangling intent) + a
// task_redelivered audit event, both attributed to the worker's CURRENT owner.
func (e *Engine) RedeliverInitialTask(ctx context.Context, workerID string) error {
	w, err := e.Store.Reader().GetWorker(workerID)
	if err != nil {
		return err
	}
	if w.State != core.WorkerRunning {
		return fmt.Errorf("%w: redeliver requires a running worker, not %s", core.ErrIllegalTransition, w.State)
	}
	if w.OwnerSession == core.PoolSessionID {
		return core.ErrProtectedPool
	}
	if w.HeadCommit != "" && w.BaseCommit != "" && w.HeadCommit != w.BaseCommit {
		return fmt.Errorf("%w: redeliver refused — worker HEAD advanced past base (the task likely already ran); verify, then arco kill + re-dispatch", core.ErrIllegalTransition)
	}
	vmc, err := e.vmFor(w.VM) // pane targets are per-host: status + prompt go to the worker's own VM
	if err != nil {
		return err
	}
	if st, _ := vmc.AgentStatus(ctx, promptTarget(w)); st == "working" || st == "blocked" {
		return fmt.Errorf("redeliver refused: agent status %q — use arco kill if it is wedged: %w", st, core.ErrAgentBusy)
	}
	// Point of no return: re-validate the load-bearing invariants under the write
	// lock and record a durable intent BEFORE the un-recallable prompt, so a
	// concurrent transfer to the pool (or a kill/pause) between the snapshot above
	// and here aborts cleanly instead of being prompted past.
	if err := e.Store.WithTx(ctx, func(tx core.Tx) error {
		cur, err := tx.GetWorker(workerID)
		if err != nil {
			return err
		}
		if cur.State != core.WorkerRunning {
			return fmt.Errorf("%w: redeliver requires a running worker, not %s", core.ErrIllegalTransition, cur.State)
		}
		if cur.OwnerSession == core.PoolSessionID {
			return core.ErrProtectedPool
		}
		// Re-check progress in-tx (fresh `cur`, zero cost) to close the sliver
		// between the snapshot HEAD gate above and the intent; best-effort either way.
		if cur.HeadCommit != "" && cur.BaseCommit != "" && cur.HeadCommit != cur.BaseCommit {
			return fmt.Errorf("%w: redeliver refused — worker HEAD advanced past base", core.ErrIllegalTransition)
		}
		_, _, _, e2 := tx.AppendEvent(core.Event{
			Kind: "redeliver_intent", WorkerID: workerID, SessionID: cur.OwnerSession, Actor: "operator",
			Payload: "{}",
		})
		return e2
	}); err != nil {
		return err
	}
	target := promptTarget(w)
	e.NoteSelfPaneOp(target) // arco-caused pane activity — excluded from the D9 back-off
	if err := vmc.PromptReady(ctx, target, promptIntentText(w.Task)); err != nil {
		e.errorEvent(ctx, workerID, "redeliver failed: "+err.Error())
		return err
	}
	// Resolve the intent + leave an audit marker, attributed to the CURRENT owner
	// (re-read under the write lock, not the stale snapshot).
	return e.Store.WithTx(ctx, func(tx core.Tx) error {
		cur, err := tx.GetWorker(workerID)
		if err != nil {
			return err
		}
		if _, _, _, e2 := tx.AppendEvent(core.Event{
			Kind: "prompt_delivered", WorkerID: workerID, SessionID: cur.OwnerSession, Actor: "operator",
			Payload: `{"redeliver":true}`,
		}); e2 != nil {
			return e2
		}
		_, _, _, e2 := tx.AppendEvent(core.Event{
			Kind: "task_redelivered", WorkerID: workerID, SessionID: cur.OwnerSession, Actor: "operator",
			Payload: "{}",
		})
		return e2
	})
}
