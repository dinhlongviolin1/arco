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
// `starting` worker (+ possibly an orphan worktree the sweep GCs). Additive —
// it does NOT touch the prompt-based Dispatch path; the API routes here only
// when a repo is supplied. NOTE: with the default Fake VMClient this runs
// end-to-end; the real LocalVMClient.Launch backend is Task-S-gated (a stub),
// so repo-based spawn against real herdr awaits that wiring.
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
		_, _, _, err := tx.AppendEvent(core.Event{
			Kind: "dispatch_intent", SessionID: sessionID, WorkerID: workerID, Actor: "cli",
			Payload: fmt.Sprintf(`{"task":%q,"workspace":%q,"repo":%q}`, task, workspace, repo),
		})
		return err
	})
	if err != nil {
		return DispatchResult{}, err
	}

	// Phase 2: external side effects (provision → quarantine → compile → launch).
	ref, wt, head, perr := e.provisionAndLaunch(ctx, workspace, repo, base, workerID, granted)

	// Phase 3: durable result + state.
	finalState := core.WorkerRunning
	if perr != nil {
		finalState = core.WorkerFailed
	}
	err = e.Store.WithTx(ctx, func(tx core.Tx) error {
		w, err := tx.GetWorker(workerID)
		if err != nil {
			return err
		}
		payload := "{}"
		if perr != nil {
			payload = fmt.Sprintf(`{"error":%q}`, perr.Error())
		} else if err := tx.BindLaunch(workerID, wt, head, ref); err != nil {
			return err
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
// ref, worktree path, and checked-out head. Any step failing aborts (the worker
// is marked failed by the caller); a partial worktree is left for sweep GC.
func (e *Engine) provisionAndLaunch(ctx context.Context, workspace, repo, base, workerID string, granted map[string]bool) (ref, wt, head string, err error) {
	root := filepath.Join(e.ConfigDir, workerID)
	wt = filepath.Join(root, "worktree")
	cfgDir := filepath.Join(root, "cfg") // sibling of the worktree (config OUTSIDE it, B6)

	head, err = worktree.Provision(ctx, e.GitBin, repo, base, wt)
	if err != nil {
		return "", wt, "", fmt.Errorf("provision: %w", err)
	}
	if _, err = quarantine.Run(wt, e.GitBin); err != nil {
		return "", wt, head, fmt.Errorf("quarantine: %w", err)
	}
	cat, err := e.Store.Reader().Catalog()
	if err != nil {
		return "", wt, head, err
	}
	if err = permcompile.Compile(cfgDir, wt, granted, cat); err != nil {
		return "", wt, head, fmt.Errorf("permcompile: %w", err)
	}
	spec := core.LaunchSpec{
		Name: workspace, Kind: "claude", Workdir: wt,
		Args: permcompile.LaunchArgs(cfgDir, granted, cat),
		Env:  spawnenv.Scrub(os.Environ()),
	}
	ref, err = e.VM.Launch(ctx, spec)
	if err != nil {
		return "", wt, head, fmt.Errorf("launch: %w", err)
	}
	return ref, wt, head, nil
}
