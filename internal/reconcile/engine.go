// Package reconcile is the application layer: it wires the ledger, the VMClient,
// and (later) the brain into the supervise loop. Dispatch is crash-safe
// (intent→execute→result); event application is deterministic via fusion, with
// the brain reserved for genuinely ambiguous states (added in a later pass).
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/oklog/ulid/v2"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/fusion"
)

// Engine holds the dependencies the supervise loop needs.
type Engine struct {
	Store core.Store
	VM    core.VMClient

	// MissThreshold is how many consecutive sweeps a worker may be unobserved
	// before it is finalized (suspect_missing → lost / completed_candidate).
	MissThreshold int

	mu     sync.Mutex
	misses map[string]int // workerID → consecutive missed sweeps (in-memory)
}

// New builds an Engine with default thresholds.
func New(store core.Store, vm core.VMClient) *Engine {
	return &Engine{Store: store, VM: vm, MissThreshold: 3, misses: map[string]int{}}
}

// DispatchResult reports what a dispatch created.
type DispatchResult struct {
	SessionID string
	WorkerID  string
}

// Dispatch creates (or reuses) a session and spawns a worker for task, crash-safe:
// pre-generate the ULID + deterministic workspace, write dispatch_intent +
// CreateWorker(starting) BEFORE the external side effect, launch via the VM, then
// dispatch_done + transition to running. A crash between intent and done leaves a
// recoverable worker with no dispatch_done (boot recovery re-drives — later pass).
func (e *Engine) Dispatch(ctx context.Context, sessionRef, task string, newSession bool) (DispatchResult, error) {
	workerID := ulid.Make().String()
	workspace := "arco_" + workerID
	var sessionID string

	// Phase 1: durable intent + worker row (before any external side effect).
	err := e.Store.WithTx(ctx, func(tx core.Tx) error {
		if newSession {
			sessionID = ulid.Make().String()
			if err := tx.CreateSession(core.Session{
				ID: sessionID, Goal: task, Status: core.SessionActive, Kind: core.SessionKindWork,
			}); err != nil {
				return err
			}
		} else {
			s, err := tx.ResolveSession(sessionRef)
			if err != nil {
				return err
			}
			if s.Kind == core.SessionKindPool {
				return core.ErrProtectedPool
			}
			sessionID = s.ID
		}
		// Worker row first so the intent event's FK (events.worker_id) is satisfied;
		// both writes are in this one atomic tx, so crash-safety is unaffected.
		if err := tx.CreateWorker(core.Worker{
			ID: workerID, OwnerSession: sessionID, State: core.WorkerStarting,
			Workspace: workspace, Task: task, RunReason: "dispatch",
		}); err != nil {
			return err
		}
		_, _, _, err := tx.AppendEvent(core.Event{
			Kind: "dispatch_intent", SessionID: sessionID, WorkerID: workerID, Actor: "cli",
			Payload: fmt.Sprintf(`{"task":%q,"workspace":%q}`, task, workspace),
		})
		return err
	})
	if err != nil {
		return DispatchResult{}, err
	}

	// Phase 2: external side effect (launch the agent). The fake VMClient in
	// tests is a no-op; a real one shells out via clavis/herdr.
	launchErr := e.VM.Prompt(ctx, workspace, task)

	// Phase 3: durable result + state.
	err = e.Store.WithTx(ctx, func(tx core.Tx) error {
		w, err := tx.GetWorker(workerID)
		if err != nil {
			return err
		}
		if launchErr != nil {
			return tx.TransitionWorker(workerID, core.WorkerFailed, w.Rev, core.Event{
				Kind: "dispatch_done", WorkerID: workerID, SessionID: sessionID,
				Payload: fmt.Sprintf(`{"error":%q}`, launchErr.Error()),
			})
		}
		return tx.TransitionWorker(workerID, core.WorkerRunning, w.Rev, core.Event{
			Kind: "dispatch_done", WorkerID: workerID, SessionID: sessionID, Payload: "{}",
		})
	})
	if err != nil {
		return DispatchResult{}, err
	}
	return DispatchResult{SessionID: sessionID, WorkerID: workerID}, nil
}

// EventInput is a normalized worker state-change from the herdr hook / intake.
type EventInput struct {
	WorkerID     string
	HerdrState   string
	Alive        bool
	ObservedHead string
	WaitingInput bool
}

// ApplyEvent reconciles one worker against an observation: record the
// observation, fuse to a target state, and transition under a CAS if it changed.
// Deterministic (no brain call); ambiguity is left for a later brain pass.
func (e *Engine) ApplyEvent(ctx context.Context, in EventInput) error {
	return e.Store.WithTx(ctx, func(tx core.Tx) error {
		w, err := tx.GetWorker(in.WorkerID)
		if err != nil {
			return err
		}
		headChanged := in.ObservedHead != "" && in.ObservedHead != w.HeadCommit
		if err := tx.ObserveWorker(in.WorkerID, core.WorkerObservation{HeadCommit: in.ObservedHead}); err != nil {
			return err
		}
		target, ambiguous := fusion.Resolve(w.State, fusion.Signals{
			HerdrState: in.HerdrState, Alive: in.Alive, HeadChanged: headChanged, WaitingInput: in.WaitingInput,
		})
		if ambiguous || target == w.State {
			return nil // nothing conclusive to change (brain classification is a later pass)
		}
		if !core.LegalWorkerTransition(w.State, target) {
			return nil // keep current; illegal target means our signals lag reality
		}
		err = tx.TransitionWorker(in.WorkerID, target, w.Rev, core.Event{
			Kind: "state_change", WorkerID: in.WorkerID, SessionID: w.OwnerSession,
			Payload: fmt.Sprintf(`{"herdr_state":%q,"target":%q}`, in.HerdrState, target),
		})
		if errors.Is(err, core.ErrRevMismatch) {
			return nil // another writer moved it first; the sweep will re-derive
		}
		return err
	})
}
