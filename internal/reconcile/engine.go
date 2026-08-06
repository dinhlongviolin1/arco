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
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/dinhlongviolin1/arco/internal/brain"
	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/fusion"
)

// BrainCfg configures the short-lived decision brain. Enabled=false (default)
// keeps the reconciler deterministic-only (no brain calls); tests and the daemon
// enable it and inject a Runner.
type BrainCfg struct {
	Enabled bool
	Profile string
	Model   string
	Runner  brain.Runner // injected; nil → clavis DefaultRunner
}

// Engine holds the dependencies the supervise loop needs.
type Engine struct {
	Store core.Store
	VM    core.VMClient
	Exec  *Exec // per-worker serialized off-write-path work
	Brain BrainCfg
	// BgCtx is the long-lived context for off-write-path (Exec) work — NOT a
	// request ctx (which would cancel when an intake HTTP request returns). The
	// daemon sets it to a ctx it cancels on shutdown; nil → context.Background().
	BgCtx context.Context
	// Redact scrubs the brain prompt before it leaves for a third-party LLM (the
	// biggest exfil surface, build-guide B4). nil → no scrub.
	Redact core.Scrubber

	// MissThreshold is how many consecutive sweeps a worker may be unobserved
	// before it is finalized (suspect_missing → lost / completed_candidate).
	MissThreshold int

	// PoolTTL is how long a released worker may sit unclaimed in the pool before
	// the sweep pauses it (0 = reaping disabled). Set from config by the daemon.
	PoolTTL time.Duration

	// BrainRate caps brain calls per session per minute (0 = unlimited). Guards
	// clavis/provider rate quotas against a single session storming the brain.
	BrainRate int

	// MaxChildren caps the number of active workers a session may own — the
	// delegation fan-in cap (0 = unlimited). Set from config by the daemon.
	MaxChildren int
	// MaxDepth is the maximum delegation depth (depth-2 supersession). 0 → 2.
	MaxDepth int

	// DefaultVM is the VM a newly-dispatched worker is assigned to ("" = unassigned).
	// MaxWorkersPerVM caps concurrent (non-terminal) workers on a VM (0 = unlimited);
	// the cap is enforced only when a worker has a non-empty VM. Set from config.
	DefaultVM       string
	MaxWorkersPerVM int

	// RollupInterval coalesces supersession rollups: at most one rollup brain call
	// per session per interval when its children complete (0 = rollup disabled).
	RollupInterval time.Duration

	// ConfigDir is the daemon-owned base for per-worker worktrees + compiled
	// configs (OUTSIDE the worktree, B6). Required by Spawn; set by the daemon.
	ConfigDir string
	// GitBin is the git binary for worktree provisioning ("" → "git").
	GitBin string

	mu     sync.Mutex
	misses map[string]int // workerID → consecutive missed sweeps (in-memory)
}

// New builds an Engine with default thresholds and an Exec (brain disabled).
// MaxChildren defaults to a bounded fan-in (defense-in-depth: an Engine built
// outside the daemon still can't spawn unbounded children); the daemon overrides
// it from config. MaxDepth's 0→2 default is applied in Delegate.
func New(store core.Store, vm core.VMClient) *Engine {
	return &Engine{Store: store, VM: vm, Exec: NewExec(4), MissThreshold: 3,
		MaxChildren: 8, misses: map[string]int{}}
}

// DispatchResult reports what a dispatch created. State is the worker's state
// after the launch attempt (running, or failed if the launch failed) so callers
// can distinguish "launched" from "created-then-failed" without a follow-up list.
type DispatchResult struct {
	SessionID string
	WorkerID  string
	State     core.WorkerState
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
		// Per-VM concurrency admission (race-free in this create tx under the
		// single-writer lock) before the worker row exists.
		if err := e.admitVM(tx, e.DefaultVM); err != nil {
			return err
		}
		// Worker row first so the intent event's FK (events.worker_id) is satisfied;
		// both writes are in this one atomic tx, so crash-safety is unaffected.
		if err := tx.CreateWorker(core.Worker{
			ID: workerID, OwnerSession: sessionID, State: core.WorkerStarting,
			VM: e.DefaultVM, Workspace: workspace, Task: task, RunReason: "dispatch",
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

	// Phases 2+3: launch the agent + durable result/state.
	finalState, err := e.launchAndFinalize(ctx, workerID, workspace, sessionID, task)
	if err != nil {
		return DispatchResult{}, err
	}
	return DispatchResult{SessionID: sessionID, WorkerID: workerID, State: finalState}, nil
}

// admitVM enforces the per-VM concurrency cap inside a create tx (race-free
// under the single-writer lock). No-op when the worker has no VM assigned or the
// cap is unset — so it's inert until a VM-assigning deployment configures both.
// NB: the cap bounds workers THIS ledger tracks on the VM label, not the
// physical host — out-of-band launches on the same machine aren't counted
// (correct under the intended single-daemon/single-ledger deployment).
func (e *Engine) admitVM(tx core.Tx, vm string) error {
	if vm == "" || e.MaxWorkersPerVM <= 0 {
		return nil
	}
	n, err := tx.CountActiveWorkersOnVM(vm)
	if err != nil {
		return err
	}
	if n >= e.MaxWorkersPerVM {
		return core.ErrVMAtCapacity
	}
	return nil
}

// launchAndFinalize performs the external launch (phase 2) then the durable
// dispatch_done transition (phase 3) shared by Dispatch and Delegate. A launch
// error is resolved by liveness (the agent may have spawned before the error
// surfaced) rather than blindly marking failed over a live process.
func (e *Engine) launchAndFinalize(ctx context.Context, workerID, workspace, sessionID, task string) (core.WorkerState, error) {
	launchErr := e.VM.Prompt(ctx, workspace, task)
	finalState := core.WorkerRunning
	if launchErr != nil {
		finalState = core.WorkerFailed
		if agents, aerr := e.VM.ListAgents(ctx); aerr == nil {
			for _, a := range agents {
				if a.Alive && a.Workspace == workspace {
					finalState = core.WorkerRunning
					break
				}
			}
		}
	}
	err := e.Store.WithTx(ctx, func(tx core.Tx) error {
		w, err := tx.GetWorker(workerID)
		if err != nil {
			return err
		}
		payload := "{}"
		if launchErr != nil {
			payload = fmt.Sprintf(`{"error":%q}`, launchErr.Error())
		}
		return tx.TransitionWorker(workerID, finalState, w.Rev, core.Event{
			Kind: "dispatch_done", WorkerID: workerID, SessionID: sessionID, Payload: payload,
		})
	})
	return finalState, err
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
	var ambiguous bool
	err := e.Store.WithTx(ctx, func(tx core.Tx) error {
		w, err := tx.GetWorker(in.WorkerID)
		if err != nil {
			return err
		}
		headChanged := in.ObservedHead != "" && in.ObservedHead != w.HeadCommit
		if err := tx.ObserveWorker(in.WorkerID, core.WorkerObservation{HeadCommit: in.ObservedHead}); err != nil {
			return err
		}
		target, amb := fusion.Resolve(w.State, fusion.Signals{
			HerdrState: in.HerdrState, Alive: in.Alive, HeadChanged: headChanged, WaitingInput: in.WaitingInput,
		})
		// A `blocked` worker is parked (e.g. by a brain billing wall) — do NOT
		// re-invoke the brain on the next ambiguous signal, or a parked worker
		// becomes a clavis/quota storm. It stays parked until an explicit reopen.
		ambiguous = amb && !w.State.Terminal() && w.State != core.WorkerBlocked
		if amb || target == w.State {
			return nil // ambiguity is handled off the write path, post-commit
		}
		if !core.LegalWorkerTransition(w.State, target) {
			return nil // keep current; illegal target means our signals lag reality
		}
		if err := tx.TransitionWorker(in.WorkerID, target, w.Rev, core.Event{
			Kind: "state_change", WorkerID: in.WorkerID, SessionID: w.OwnerSession,
			Payload: fmt.Sprintf(`{"herdr_state":%q,"target":%q}`, in.HerdrState, target),
		}); err != nil {
			if errors.Is(err, core.ErrRevMismatch) {
				return nil // another writer moved it first; the sweep will re-derive
			}
			return err
		}
		// A worker awaiting input surfaces a question escalation (shadow: the
		// operator decides; no unattended auto-answer in P2). One-pending-per-worker
		// is enforced by OpenEscalation, so repeat events don't pile up.
		if target == core.WorkerWaitingForUser {
			_, err := tx.OpenEscalation(core.Escalation{
				WorkerID: in.WorkerID, SessionID: w.OwnerSession, Kind: "question",
				QuestionClass: "clarify", ActionClass: core.ClassAmbiguous, Tier: core.TierMedium,
				Action: "worker is awaiting input",
			})
			return err
		}
		// If the worker LEFT a waiting state by another path (e.g. a later herdr
		// signal), close any lingering pending escalation so it isn't a phantom.
		if isWaiting(w.State) && !isWaiting(target) {
			if _, err := tx.ExpirePendingForWorker(in.WorkerID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	// Ambiguous signals → ask the brain to classify, OFF the write path, serialized
	// per worker via Exec (Submit fires strictly after the commit above; no
	// reentrancy). Deterministic states never reach the brain.
	if ambiguous && e.Brain.Enabled && e.Exec != nil {
		wid := in.WorkerID
		e.Exec.Submit(wid, func() { e.brainClassify(e.bg(), wid) })
	}
	return nil
}

// bg returns the long-lived background context for off-write-path work.
func (e *Engine) bg() context.Context {
	if e.BgCtx != nil {
		return e.BgCtx
	}
	return context.Background()
}

func isWaiting(s core.WorkerState) bool {
	return s == core.WorkerWaitingForUser || s == core.WorkerWaitingConfirmation
}
