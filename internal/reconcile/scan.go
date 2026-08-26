package reconcile

import (
	"context"
	"fmt"
	"sort"

	"github.com/oklog/ulid/v2"

	"github.com/dinhlongviolin1/arco/internal/core"
)

// ScannedAgent is one live agent observed on a VM, annotated with whether arco
// already tracks it (a non-terminal worker whose ref/workspace matches). Adopt
// turns an untracked one into a monitored worker.
type ScannedAgent struct {
	core.AgentObs
	VM       string // arco VM name ("" = the local box)
	Tracked  bool   // already an arco worker
	WorkerID string // the tracking worker, when Tracked
}

// ScanAgents lists LIVE agents across every known VM and marks which arco already
// tracks. This is the orchestrator's window into herdr sessions arco did not
// launch — the answer to "what's actually running out there". Read-only; a
// per-VM ListAgents error skips that VM rather than failing the whole scan (one
// unreachable host must not blind the operator to the rest).
func (e *Engine) ScanAgents(ctx context.Context) ([]ScannedAgent, error) {
	workers, err := e.Store.Reader().ListWorkers(core.WorkerFilter{})
	if err != nil {
		return nil, err
	}
	// Index the non-terminal workers so a scanned agent can be marked tracked by
	// either its backend ref (arco-launched) or its workspace (fallback).
	byRef, byWS := map[string]string{}, map[string]string{}
	for _, w := range workers {
		if w.State.Terminal() {
			continue
		}
		if w.AgentRef != "" {
			byRef[w.AgentRef] = w.ID
		}
		if w.Workspace != "" {
			byWS[w.Workspace] = w.ID
		}
	}
	var out []ScannedAgent
	for _, vmName := range e.vmNames() {
		c, cerr := e.vmFor(vmName)
		if cerr != nil || c == nil {
			continue
		}
		agents, aerr := c.ListAgents(ctx)
		if aerr != nil {
			continue // best-effort per VM
		}
		for _, a := range agents {
			if !a.Alive {
				continue
			}
			wid := byRef[a.Ref]
			if wid == "" {
				wid = byWS[a.Workspace]
			}
			out = append(out, ScannedAgent{AgentObs: a, VM: vmName, Tracked: wid != "", WorkerID: wid})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].VM != out[j].VM {
			return out[i].VM < out[j].VM
		}
		return out[i].Ref < out[j].Ref
	})
	return out, nil
}

// vmNames is the set of VMs to scan: the [[vms]] registry when routing is on,
// else just the local box ("").
func (e *Engine) vmNames() []string {
	if e.VMs == nil {
		return []string{""}
	}
	names := make([]string, 0, len(e.VMs))
	for n := range e.VMs {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Adopt registers an existing, arco-UNMANAGED herdr agent as a MONITOR-ONLY
// worker so arco tracks it internally — "the session is the same". ref is the
// agent's backend handle (herdr pane id), its workspace id, or its agent-session
// id; a fresh scan resolves it to the live agent (never adopts a stale handle).
//
// The adopted session is created in MANUAL supervision mode (observe + ledger
// only: no brain, no auto-reap, no pings) and the worker has NO worktree, creds,
// or compiled permissions — it is "uncontained": arco didn't launch it, so it
// can observe liveness, relay to it, and (operator-initiated) kill it, but it
// cannot enforce capability grants on it. The sweep then tracks it by ref like
// any other worker: if its agent exits, arco finalizes it (lost/completed) on
// the normal miss path. Idempotent-ish: re-adopting an already-tracked agent is
// refused (returns the existing worker id).
func (e *Engine) Adopt(ctx context.Context, ref string) (DispatchResult, error) {
	if ref == "" {
		return DispatchResult{}, fmt.Errorf("reconcile: Adopt requires an agent ref")
	}
	scan, err := e.ScanAgents(ctx)
	if err != nil {
		return DispatchResult{}, err
	}
	var found *ScannedAgent
	for i := range scan {
		if a := &scan[i]; a.Ref == ref || a.Workspace == ref || a.SessionID == ref {
			found = a
			break
		}
	}
	if found == nil {
		return DispatchResult{}, fmt.Errorf("reconcile: no live agent matches %q", ref)
	}
	if found.Tracked {
		return DispatchResult{WorkerID: found.WorkerID, State: core.WorkerRunning},
			fmt.Errorf("reconcile: agent %q is already tracked by worker %s", ref, found.WorkerID)
	}

	sessionID := ulid.Make().String()
	workerID := ulid.Make().String()
	title := found.Title
	if title == "" {
		kind := found.Kind
		if kind == "" {
			kind = "agent"
		}
		title = "adopted " + kind
		if found.Cwd != "" {
			title += " @ " + found.Cwd
		}
	}
	err = e.Store.WithTx(ctx, func(tx core.Tx) error {
		if err := tx.CreateSession(core.Session{
			ID: sessionID, Goal: title, Title: title, Status: core.SessionActive,
			Kind: core.SessionKindWork, SupervisionMode: core.ModeManual,
		}); err != nil {
			return err
		}
		if err := tx.CreateWorker(core.Worker{
			ID: workerID, OwnerSession: sessionID, State: core.WorkerRunning,
			VM: found.VM, Workspace: found.Workspace, AgentRef: found.Ref, BootID: found.BootID,
			AgentKind: found.Kind, Title: title, Task: title, RunReason: "adopt",
		}); err != nil {
			return err
		}
		_, _, _, e2 := tx.AppendEvent(core.Event{
			Kind: "adopt", SessionID: sessionID, WorkerID: workerID, Actor: "operator",
			Payload: fmt.Sprintf(`{"ref":%q,"workspace":%q,"vm":%q,"agent_session":%q}`,
				found.Ref, found.Workspace, found.VM, found.SessionID),
		})
		return e2
	})
	if err != nil {
		return DispatchResult{}, err
	}
	return DispatchResult{SessionID: sessionID, WorkerID: workerID, State: core.WorkerRunning}, nil
}
