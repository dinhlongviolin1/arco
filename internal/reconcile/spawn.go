package reconcile

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/oklog/ulid/v2"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/permcompile"
	"github.com/dinhlongviolin1/arco/internal/quarantine"
	"github.com/dinhlongviolin1/arco/internal/spawnenv"
	"github.com/dinhlongviolin1/arco/internal/worktree"
)

// Spawn is the T14 repo-based spawn pipeline: it provisions a fresh per-worker
// worktree from repo@base, quarantines it, compiles the worker's capability tree
// into a config staged OUTSIDE the worktree (B6), then LAUNCHES a new agent with
// the pinned flags + a credential-scrubbed env, capturing the backend handle for
// sweep correlation. It composes the pieces built as PASS-3 prerequisites.
//
// Crash-safe like Dispatch: dispatch_intent + the worker row commit BEFORE the
// external side effects (clone/launch); a crash between leaves a recoverable
// `starting` worker (boot Recover resolves by liveness) + an orphan per-worker
// dir (cleaned up on a pre-launch *error*; a *crash* orphan awaits a sweep-GC
// follow-up — there is no GC today). Additive —
// it does NOT touch the prompt-based Dispatch path; the API routes here only
// when a repo is supplied. NOTE: the real LocalVMClient.Launch backend is now
// implemented against the confirmed herdr 0.7.5 contract (workspace create →
// list → agent start); live end-to-end verification against a running herdr is
// user-gated (spawning a real agent is an outward, quota-consuming side effect).
func (e *Engine) Spawn(ctx context.Context, sessionRef, task string, newSession bool, repo, base string) (DispatchResult, error) {
	if repo == "" {
		return DispatchResult{}, fmt.Errorf("reconcile: Spawn requires a repo (use Dispatch for the prompt path)")
	}
	if e.ConfigDir == "" {
		return DispatchResult{}, fmt.Errorf("reconcile: Spawn requires Engine.ConfigDir")
	}
	workerID := ulid.Make().String()
	workspace := "arco_" + workerID
	var sessionID string
	var granted map[string]bool
	var leaseID string // function-scoped so phase 3 can release it on a failed spawn

	// Phase 1: durable intent + worker row + granted-set snapshot (atomic).
	err := e.Store.WithTx(ctx, func(tx core.Tx) error {
		if newSession {
			sessionID = ulid.Make().String()
			if err := tx.CreateSession(core.Session{
				ID: sessionID, Goal: task, Status: core.SessionActive, Kind: core.SessionKindWork, Repo: repo,
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
		if err := e.admitVM(tx, e.DefaultVM); err != nil {
			return err
		}
		// Provider-pool concurrency lease, acquired BEFORE the intent (admission is
		// race-free in this single-writer tx). ErrLeaseRejected aborts the whole tx
		// → no worker created → caller backs off. Inert when DefaultPool is unset.
		if e.DefaultPool != "" {
			leaseID = ulid.Make().String()
			if err := tx.AcquireLease(leaseID, e.DefaultPool, e.LeaseTTL); err != nil {
				leaseID = "" // not acquired (tx will roll back anyway)
				return err
			}
		}
		if err := tx.CreateWorker(core.Worker{
			ID: workerID, OwnerSession: sessionID, State: core.WorkerStarting, VM: e.DefaultVM,
			Workspace: workspace, Task: task, RunReason: "spawn", AgentKind: "claude",
		}); err != nil {
			return err
		}
		var gerr error
		if granted, gerr = tx.GrantedCapabilities(sessionID); gerr != nil {
			return gerr
		}
		cursor, _, _, err := tx.AppendEvent(core.Event{
			Kind: "dispatch_intent", SessionID: sessionID, WorkerID: workerID, Actor: "cli",
			Payload: fmt.Sprintf(`{"task":%q,"workspace":%q,"repo":%q}`, task, workspace, repo),
		})
		if err != nil {
			return err
		}
		if leaseID != "" { // bind the lease to the worker + its intent event
			return tx.BindLease(leaseID, workerID, cursor)
		}
		return nil
	})
	if err != nil {
		return DispatchResult{}, err
	}

	// Phase 2: external side effects (provision → quarantine → compile → launch).
	ref, wt, head, perr := e.provisionAndLaunch(ctx, workspace, repo, base, workerID, granted)

	// Phase 3: durable result + state. On a Phase-2 error the launch may still have
	// spawned the agent before erroring (ref-capture timeout, transient post-spawn
	// error) — resolve by liveness like Dispatch rather than blindly marking a live
	// agent's worker terminal (which would leave it running unmanaged forever). A
	// provision/compile failure launched nothing → no agent matches → failed.
	finalState := core.WorkerRunning
	if perr != nil {
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
	err = e.Store.WithTx(ctx, func(tx core.Tx) error {
		w, err := tx.GetWorker(workerID)
		if err != nil {
			return err
		}
		payload := "{}"
		if perr != nil {
			payload = fmt.Sprintf(`{"error":%q}`, perr.Error())
		}
		// Free the pool slot NOW on a FAILED spawn (finalState, not perr — a
		// launch-error-but-alive worker is adopted running and KEEPS its lease)
		// rather than waiting for the sweep's terminal-worker reaper — avoids a
		// spurious at-capacity reject on an immediate retry. Idempotent; no-op when
		// no pool.
		if finalState == core.WorkerFailed && leaseID != "" {
			if err := tx.ReleaseLease(leaseID); err != nil {
				return err
			}
		}
		if finalState == core.WorkerRunning {
			// ref is unrecoverable when Launch errored → bind "" so the sweep
			// correlates this adopted worker by workspace instead.
			r := ref
			if perr != nil {
				r = ""
			}
			if err := tx.BindLaunch(workerID, wt, head, r); err != nil {
				return err
			}
		}
		return tx.TransitionWorker(workerID, finalState, w.Rev, core.Event{
			Kind: "dispatch_done", WorkerID: workerID, SessionID: sessionID, Payload: payload,
		})
	})
	if err != nil {
		return DispatchResult{}, err
	}
	return DispatchResult{SessionID: sessionID, WorkerID: workerID, State: finalState}, nil
}

// provisionAndLaunch does the external spawn side effects and returns the backend
// ref, worktree path, and checked-out head. A failure BEFORE launch removes the
// partial per-worker dir (no live agent can exist yet, so cleanup is safe); a
// LAUNCH failure leaves the dir (the agent may be live and adopted by the caller).
// NB: a crash mid-provision still orphans the dir — a sweep-side GC of terminal
// workers' ConfigDir subtrees is a documented follow-up (there is no GC today).
func (e *Engine) provisionAndLaunch(ctx context.Context, workspace, repo, base, workerID string, granted map[string]bool) (ref, wt, head string, err error) {
	root := filepath.Join(e.ConfigDir, workerID)
	wt = filepath.Join(root, "worktree")
	cfgDir := filepath.Join(root, "cfg") // sibling of the worktree (config OUTSIDE it, B6)
	// cleanup removes the partial per-worker dir on a pre-launch failure.
	cleanup := func(e error, stage string) (string, string, string, error) {
		_ = worktree.Remove(root)
		return "", wt, head, fmt.Errorf("%s: %w", stage, e)
	}

	head, err = worktree.Provision(ctx, e.GitBin, repo, base, wt)
	if err != nil {
		head = ""
		return cleanup(err, "provision")
	}
	if _, err = quarantine.Run(wt, e.GitBin); err != nil {
		return cleanup(err, "quarantine")
	}
	cat, err := e.Store.Reader().Catalog()
	if err != nil {
		return cleanup(err, "catalog")
	}
	if err = permcompile.Compile(cfgDir, wt, granted, cat); err != nil {
		return cleanup(err, "permcompile")
	}
	spec := core.LaunchSpec{
		Name: workspace, Kind: "claude", Workdir: wt,
		Args: permcompile.LaunchArgs(cfgDir, granted, cat),
		Env:  spawnenv.Scrub(os.Environ()),
	}
	ref, err = e.VM.Launch(ctx, spec)
	if err != nil {
		// Do NOT remove — the agent may have spawned; the caller resolves by liveness.
		return "", wt, head, fmt.Errorf("launch: %w", err)
	}
	return ref, wt, head, nil
}
