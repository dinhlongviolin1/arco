package reconcile

import (
	"context"
	"fmt"

	"github.com/oklog/ulid/v2"

	"github.com/dinhlongviolin1/arco/internal/core"
)

// defaultMaxDepth is the depth-2 supersession ceiling (root=0, children 1 and 2).
const defaultMaxDepth = 2

// Delegate spawns a CHILD worker for a subtask on behalf of parentWorkerID —
// same crash-safe intent→launch→done shape as Dispatch, but the admission
// (delegation depth + per-session fan-in) is checked INSIDE the create tx under
// the single-writer lock, so a burst of concurrent delegations in one session
// can't exceed the fan-in cap. The child inherits the parent's owner session and
// records parent_worker_id + delegation_depth = parent+1.
//
// Returns ErrMaxDepthExceeded / ErrFanInExceeded (admission denied),
// ErrIllegalTransition (parent already terminal), or a store/launch error.
func (e *Engine) Delegate(ctx context.Context, parentWorkerID, task string) (DispatchResult, error) {
	childID := ulid.Make().String()
	workspace := "arco_" + childID
	var sessionID string
	var childDepth int

	// Phase 1: admission + durable intent + child row (all atomic).
	err := e.Store.WithTx(ctx, func(tx core.Tx) error {
		parent, err := tx.GetWorker(parentWorkerID)
		if err != nil {
			return err
		}
		if parent.State.Terminal() {
			return fmt.Errorf("%w: parent worker is %s", core.ErrIllegalTransition, parent.State)
		}
		maxDepth := e.MaxDepth
		if maxDepth == 0 {
			maxDepth = defaultMaxDepth
		}
		childDepth = parent.DelegationDepth + 1
		if childDepth > maxDepth {
			return core.ErrMaxDepthExceeded
		}
		sessionID = parent.OwnerSession
		// A pool-owned (handoff-released) parent must not spawn children INTO the
		// protected pool (Dispatch/claim/transfer already reject the pool; Delegate
		// was the odd one out — opus review). Defense-in-depth: the brain gate above
		// already stops a pooled worker from reaching a `dispatch` StepResult.
		if sessionID == core.PoolSessionID {
			return core.ErrProtectedPool
		}
		if e.MaxChildren > 0 {
			n, err := tx.CountActiveWorkers(sessionID)
			if err != nil {
				return err
			}
			if n >= e.MaxChildren {
				return core.ErrFanInExceeded
			}
		}
		if err := e.admitVM(tx, e.DefaultVM); err != nil { // per-VM concurrency cap
			return err
		}
		// Child row first so the intent event's worker_id FK is satisfied.
		if err := tx.CreateWorker(core.Worker{
			ID: childID, OwnerSession: sessionID, State: core.WorkerStarting,
			VM: e.DefaultVM, Workspace: workspace, Task: task, RunReason: "delegate",
			ParentWorkerID: parentWorkerID, DelegationDepth: childDepth,
		}); err != nil {
			return err
		}
		_, _, _, err = tx.AppendEvent(core.Event{
			Kind: "dispatch_intent", SessionID: sessionID, WorkerID: childID, Actor: "brain",
			Payload: fmt.Sprintf(`{"task":%q,"workspace":%q,"parent":%q,"depth":%d}`, task, workspace, parentWorkerID, childDepth),
		})
		return err
	})
	if err != nil {
		return DispatchResult{}, err
	}

	// Phases 2+3: launch the child + durable result/state (shared with Dispatch).
	finalState, err := e.launchAndFinalize(ctx, childID, workspace, sessionID, task)
	if err != nil {
		return DispatchResult{}, err
	}
	return DispatchResult{SessionID: sessionID, WorkerID: childID, State: finalState}, nil
}
