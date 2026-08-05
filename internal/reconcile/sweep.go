package reconcile

import (
	"context"
	"errors"
	"fmt"

	"github.com/dinhlongviolin1/arco/internal/core"
)

// SweepResult summarizes one sweep pass (handy for logging/tests).
type SweepResult struct {
	Observed     int
	Transitions  int
	LeasesReaped int // leaked/stale provider-pool leases released (B10-lease)
}

// Sweep is the authoritative periodic repair (build-guide PASS-2 / Task 7).
// Push (intake) is an optimization over this. For each non-terminal worker it
// observes liveness (via VMClient identity, never a central PID) and git HEAD,
// then:
//   - alive → reset the miss counter, record HEAD/last_seen (state driven by push);
//   - not alive but under MissThreshold → suspect (leave as-is, count the miss);
//   - not alive at/over MissThreshold → finalize: HEAD advanced ⇒ completed_candidate
//     (made progress then vanished), else ⇒ lost.
//
// It runs even when no intake event arrived, so a dropped push never desyncs.
func (e *Engine) Sweep(ctx context.Context) (SweepResult, error) {
	var res SweepResult
	all, err := e.Store.Reader().ListWorkers(core.WorkerFilter{})
	if err != nil {
		return res, err
	}
	// Reap leaked/stale provider-pool leases first (B10-lease) — independent of
	// worker liveness, so it also runs when there are no live workers. Workers
	// finalized later in THIS sweep are reaped on the next one (eventual).
	res.LeasesReaped, _ = e.reapLeases(ctx)
	var live []core.Worker
	worktrees := map[string]bool{}
	for _, w := range all {
		if w.State.Terminal() {
			continue
		}
		live = append(live, w)
		worktrees[headKey(w)] = true
	}
	if len(live) == 0 {
		return res, nil
	}

	agents, err := e.VM.ListAgents(ctx)
	if err != nil {
		return res, err
	}
	aliveByWorkspace := map[string]core.AgentObs{}
	for _, a := range agents {
		if a.Alive {
			aliveByWorkspace[a.Workspace] = a
		}
	}
	var wts []string
	for wt := range worktrees {
		wts = append(wts, wt)
	}
	heads := map[string]string{}
	if len(wts) > 0 {
		if heads, err = e.VM.GitHeads(ctx, wts); err != nil {
			return res, err
		}
	}

	for _, w := range live {
		res.Observed++
		obs, alive := aliveByWorkspace[w.Workspace]
		// identity check: if we recorded a boot/pid-start, it must match (guards PID reuse).
		if alive && w.BootID != "" && obs.BootID != "" && obs.BootID != w.BootID {
			alive = false
		}
		headNow := heads[headKey(w)]
		headChanged := headNow != "" && headNow != w.HeadCommit

		if alive {
			e.resetMiss(w.ID)
			_ = e.Store.WithTx(ctx, func(tx core.Tx) error {
				return tx.ObserveWorker(w.ID, core.WorkerObservation{
					HeadCommit: headNow, BootID: obs.BootID, PIDStartTime: obs.PIDStartTime,
				})
			})
			continue
		}

		if e.bumpMiss(w.ID) < e.MissThreshold {
			continue // suspect_missing; give push/next sweep a chance
		}
		target := core.WorkerLost
		if headChanged {
			target = core.WorkerCompletedCandidate
		}
		changed, err := e.finalize(ctx, w, target, headNow)
		if err != nil {
			return res, err
		}
		if changed {
			res.Transitions++
			e.resetMiss(w.ID)
		}
	}
	return res, nil
}

// reapLeases releases leaked/stale provider-pool leases in one tx (B10-lease).
func (e *Engine) reapLeases(ctx context.Context) (int, error) {
	var n int
	err := e.Store.WithTx(ctx, func(tx core.Tx) error {
		var err error
		n, err = tx.ReapLeases()
		return err
	})
	return n, err
}

func (e *Engine) finalize(ctx context.Context, w core.Worker, target core.WorkerState, headNow string) (bool, error) {
	err := e.Store.WithTx(ctx, func(tx core.Tx) error {
		cur, err := tx.GetWorker(w.ID)
		if err != nil {
			return err
		}
		if cur.State.Terminal() || !core.LegalWorkerTransition(cur.State, target) {
			return nil
		}
		if headNow != "" {
			if err := tx.ObserveWorker(w.ID, core.WorkerObservation{HeadCommit: headNow}); err != nil {
				return err
			}
		}
		e := tx.TransitionWorker(w.ID, target, cur.Rev, core.Event{
			Kind: "reconcile", WorkerID: w.ID, SessionID: cur.OwnerSession,
			Payload: fmt.Sprintf(`{"sweep":true,"target":%q}`, target),
		})
		return e
	})
	if errors.Is(err, core.ErrRevMismatch) {
		return false, nil
	}
	return err == nil, err
}

// headKey is the git-HEAD lookup key for a worker: its worktree if known, else
// its workspace (a freshly-dispatched worker has no worktree yet, so keying only
// on worktree would make completed_candidate unreachable — always lost).
func headKey(w core.Worker) string {
	if w.Worktree != "" {
		return w.Worktree
	}
	return w.Workspace
}

func (e *Engine) bumpMiss(id string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.misses[id]++
	return e.misses[id]
}

func (e *Engine) resetMiss(id string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.misses, id)
}

// Recover is boot recovery (survive-and-reconcile, build-guide PASS-2 / Task 18).
// A worker still in `starting` at boot had a dispatch_intent but the dispatch_done
// never landed (crash mid-launch): resolve it by liveness — alive ⇒ running
// (adopt), not alive ⇒ failed (never double-spawn). Then run one Sweep so a
// `running` worker whose process died while we were down is repaired immediately.
func (e *Engine) Recover(ctx context.Context) error {
	starting, err := e.Store.Reader().ListWorkers(core.WorkerFilter{State: core.WorkerStarting})
	if err != nil {
		return err
	}
	agents, err := e.VM.ListAgents(ctx)
	if err != nil {
		return err
	}
	aliveByWorkspace := map[string]core.AgentObs{}
	for _, a := range agents {
		if a.Alive {
			aliveByWorkspace[a.Workspace] = a
		}
	}
	var firstErr error
	for _, w := range starting {
		obs, alive := aliveByWorkspace[w.Workspace]
		// same PID-reuse identity guard as Sweep: don't adopt a recycled workspace.
		if alive && w.BootID != "" && obs.BootID != "" && obs.BootID != w.BootID {
			alive = false
		}
		target := core.WorkerFailed
		reason := "boot: dispatch_intent without dispatch_done, process not found"
		if alive {
			target, reason = core.WorkerRunning, "boot: adopted live worker"
		}
		if err := e.Store.WithTx(ctx, func(tx core.Tx) error {
			cur, err := tx.GetWorker(w.ID)
			if err != nil {
				return err
			}
			if cur.State != core.WorkerStarting {
				return nil
			}
			return tx.TransitionWorker(w.ID, target, cur.Rev, core.Event{
				Kind: "reconcile", WorkerID: w.ID, SessionID: cur.OwnerSession,
				Payload: fmt.Sprintf(`{"boot_recovery":true,"reason":%q}`, reason),
			})
		}); err != nil && firstErr == nil {
			firstErr = err // record but keep going: one bad worker mustn't skip the rest + the sweep
		}
	}
	// Always run the sweep, even if a per-worker recovery failed, so live-but-
	// dead `running` workers are repaired.
	if _, err := e.Sweep(ctx); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}
