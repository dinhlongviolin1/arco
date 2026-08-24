package reconcile

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/notify"
)

// brainCallTimeout mirrors the brain.Invoke default (invoke.go): the wall-clock
// ceiling on one classification. The stale-intent grace must exceed it so a
// still-in-flight call is never mistaken for a crash-lost one.
const brainCallTimeout = 120 * time.Second

// defaultBrainIntentGrace bounds how old a dangling brain_intent must be before
// the sweep re-drives it. 2.5× the call timeout — comfortably past an in-flight
// call, and the floor a misconfigured BrainIntentGrace is clamped up to.
const defaultBrainIntentGrace = 5 * time.Minute

// SweepResult summarizes one sweep pass (handy for logging/tests).
type SweepResult struct {
	Observed            int
	Transitions         int
	LeasesReaped        int // leaked/stale provider-pool leases released (B10-lease)
	PooledPaused        int // workers paused after sitting unclaimed past the pool TTL
	RollupsTriggered    int // parents with completed children enqueued for a coalesced rollup
	BrainRedrives       int // dangling brain_intents (crash-lost calls) re-submitted
	EscalationsTimedOut int // pending escalations expired past EscalationTimeout (+ worker paused)
	AgentsReaped        int // orphaned agents of TERMINAL workers stopped (quota reclaim)
	ActivityRestored    int // sessions the human-activity back-off demoted, restored to auto after the quiet period
	AutoAnswered        int // pending drafted questions resolved by the earn-out promotion (T3.5)
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
	// The estop (operator pause) stands down every AUTONOMOUS leg of the sweep —
	// activity restores, earn-out answers, rollup brain calls, CI polling, and
	// the orphan reaper — while liveness observation and state finalization
	// continue (bookkeeping of reality, not new work). Checked once per sweep.
	paused := e.Paused()
	// Return sessions the human-activity back-off demoted to auto once their pane
	// has been quiet long enough (D9/T3.6). Session-scoped, so it runs before any
	// worker-shaped early return below. Not while paused: restoring AUTONOMY
	// under an engaged estop would be exactly the surprise estop exists to stop.
	if !paused {
		res.ActivityRestored = e.restoreActivityBackoff(ctx)
	}
	// Pause workers that have sat unclaimed in the pool past the TTL.
	if e.PoolTTL > 0 {
		res.PooledPaused, _ = e.reapPooled(ctx)
	}
	// Auto-resolve escalations the operator never answered: expire + pause the
	// waiting worker so it can't wait on a human forever (build-guide timeout →
	// auto-pause).
	if e.EscalationTimeout > 0 {
		res.EscalationsTimedOut = e.reapEscalations(ctx)
	}
	// Earn-out promotion (rev7/T3.5): resolve pending drafted questions whose
	// class has earned brain auto-answers, under the hard gates. After the
	// escalation-timeout reaper (an expired escalation is no longer promotable)
	// and before the pending-escalation snapshot below, so a worker resumed here
	// is not mistaken for paused-with-pending.
	if !paused {
		res.AutoAnswered = e.autoAnswerEarnedOut(ctx)
	}
	// Supersession rollup: re-drive any ALIVE parent whose children have
	// completed. maybeRollup coalesces to ≤1 rollup brain call per session per
	// RollupInterval, so triggering opportunistically every sweep (regardless of
	// which path made a child terminal) is safe.
	if e.RollupInterval > 0 && e.Brain.Enabled && !paused {
		res.RollupsTriggered = e.triggerRollups(all)
	}
	// Re-drive brain classifications lost to a crash in the off-write-path call
	// window (brain_intent committed, applyStep never ran). The sweep's alive path
	// only records observations — it never re-runs fusion — so absent a fresh push
	// event a lost call would otherwise never retry. Runs at boot too (Recover →
	// Sweep). Detection is correlation-based (StaleBrainIntents), so a re-drive can
	// never duplicate a prompt/child; brainClassify re-guards state/pool/rate.
	if e.Brain.Enabled && e.Exec != nil {
		res.BrainRedrives = e.redriveStaleBrainIntents()
	}
	// A worker with a PENDING escalation must keep its (live) agent even when
	// paused: an operator approval drives paused→running and re-prompts the SAME
	// pane (deliverDecision reconnect — e.g. AuditDeniedAttempt pauses a running
	// worker + opens a danger confirm). Reaping that agent would silently discard
	// the approval and lose the worker (opus review). So paused-with-pending is NOT
	// reclaimable; it stays liveness-tracked (a dead agent → finalize, since a
	// reconnect can't help). Computed AFTER reapEscalations (which expires its
	// escalation before pausing, so a timeout-paused worker IS reclaimable).
	pendingEsc := map[string]bool{}
	if escs, eerr := e.Store.Reader().ListEscalations(core.EscalationFilter{Status: "pending"}); eerr == nil {
		for _, es := range escs {
			if es.WorkerID != "" {
				pendingEsc[es.WorkerID] = true
			}
		}
	}
	if len(all) == 0 {
		return res, nil // no workers at all → skip the herdr agent-list calls entirely
	}
	// Group the workers by the VM that owns their agent (T3.3): pane ids are
	// per-host, so each worker is correlated ONLY against ITS OWN VM's ListAgents
	// (a same-ref agent on a different VM is someone else's), and GitHeads runs
	// once per VM over that VM's worktrees. With no registry there is exactly one
	// group on e.VM — today's single-client behavior unchanged.
	for _, g := range e.sweepGroups(all, pendingEsc) {
		if g.client == nil {
			continue // registry gap (unknown VM): unobservable — never finalize on it
		}
		// One agent list per VM serves both the orphan reaper (terminal workers)
		// and the liveness loop, fetched whenever the VM has ANY worker — reaping
		// an orphan must run even when every worker is terminal. A ListAgents
		// error keeps today's transient-noise posture, PER VM: this sweep observes
		// nothing for THIS VM's workers (no misses, no finalize) — one host's
		// dropped ssh must not mass-finalize its fleet. Routing off = one group,
		// so the error is the whole sweep's, exactly as before.
		agents, err := g.client.ListAgents(ctx)
		if err != nil {
			if e.VMs == nil {
				return res, err
			}
			continue
		}
		// Stop agents still alive on a worker whose agent should no longer run —
		// TERMINAL (the kill crash-orphan, or a worker finalized lost/failed with a
		// lingering pane) OR PAUSED-without-a-pending-escalation (auto-kill-on-pause:
		// a worker paused by pool-TTL / escalation-timeout has only an idle agent
		// burning quota; its worktree is preserved). Identity-strict, so it never
		// closes a stranger's recycled pane. Runs before the liveness loop and even
		// when the VM has no live workers.
		if !paused { // estop: no destructive actions at all while paused
			res.AgentsReaped += e.reapOrphanedAgents(ctx, g.client, g.all, agents, pendingEsc)
		}

		if len(g.live) == 0 {
			continue
		}
		lookupAlive := aliveLookup(agents)
		var wts []string
		for wt := range g.worktrees {
			wts = append(wts, wt)
		}
		heads := map[string]string{}
		if len(wts) > 0 {
			if heads, err = g.client.GitHeads(ctx, wts); err != nil {
				if e.VMs == nil {
					return res, err
				}
				continue // same per-VM posture: no heads → observe nothing this sweep
			}
		}

		for _, w := range g.live {
			res.Observed++
			obs, alive := lookupAlive(w)
			// identity check: if we recorded a boot/pid-start, it must match (guards PID reuse).
			if alive && w.BootID != "" && obs.BootID != "" && obs.BootID != w.BootID {
				alive = false
			}
			headNow := heads[headKey(w)]
			headChanged := headNow != "" && headNow != w.HeadCommit

			if alive {
				e.resetMiss(w.ID)
				// Identity is established ONCE, at launch (BindLaunch). Observation only
				// CONFIRMS it, never ESTABLISHES a new one: if launch-capture missed
				// (w.BootID==""), we must NOT stamp the observed agent's terminal_id onto
				// the row, because a stranger on a recycled pane would then be recorded as
				// this worker's identity and later give the DESTRUCTIVE orphan reaper a
				// false positive match (opus+qwen review — the empty-at-birth poisoning
				// window). Such a worker stays unidentifiable and the reaper declines it —
				// a non-destructive miss, the same trade made throughout MED-3.
				obsBootID := ""
				if w.BootID != "" {
					obsBootID = obs.BootID
				}
				_ = e.Store.WithTx(ctx, func(tx core.Tx) error {
					return tx.ObserveWorker(w.ID, core.WorkerObservation{
						HeadCommit: headNow, BootID: obsBootID, PIDStartTime: obs.PIDStartTime,
					})
				})
				blocked, err := e.checkStall(ctx, w, headNow)
				if err != nil {
					return res, err
				}
				if blocked {
					res.Transitions++
				}
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
			}
			// Reset the miss counter whether or not we finalized: if we did, it's moot;
			// if we couldn't (e.g. a completed_candidate — no legal edge to lost — whose
			// agent is expectedly gone, or a rev race), NOT resetting would re-bump every
			// sweep and grow the in-memory misses map without bound (whole-system audit
			// LOW-5). A still-missing worker simply re-accrues from zero next sweep.
			e.resetMiss(w.ID)
		}
	}
	// Verification leg 1 (rev7/T3.1): poll CI check-runs for completed_candidate
	// workers, after the liveness loop (a candidate's agent is expectedly gone —
	// finalize already declines it above, so candidates are always still live-
	// listed here). Best-effort evidence gathering; never a sweep error.
	if !paused {
		e.pollCICheckRuns(ctx, all)
	}
	return res, nil
}

// redriveStaleBrainIntents re-submits brainClassify (off the write path) for
// every worker whose most-recent brain_intent is dangling (unresolved by any
// cid-sibling) and older than the grace. Returns how many were re-submitted. The
// grace is floored to ≥ 2× the brain-call timeout so a still-in-flight call is
// never re-driven; brainClassify re-guards worker state, pool ownership, and the
// per-session brain-rate cap. Caller has checked Brain.Enabled && Exec != nil.
func (e *Engine) redriveStaleBrainIntents() int {
	grace := e.BrainIntentGrace
	if grace < 2*brainCallTimeout {
		grace = defaultBrainIntentGrace
	}
	ids, err := e.Store.Reader().StaleBrainIntents(e.Store.Now().Add(-grace))
	if err != nil {
		return 0 // best-effort: a read error just skips this sweep's re-drive
	}
	n := 0
	for _, id := range ids {
		// At most one in-flight re-drive per worker: the brain_intent marker that
		// takes this worker out of the dangling set is committed async (inside the
		// submitted brainClassify), so without this guard a later sweep under Exec
		// backlog would re-submit the same worker → a duplicate classification.
		if !e.claimRedrive(id) {
			continue
		}
		e.Exec.Submit(id, func() {
			defer e.releaseRedrive(id)
			e.brainClassify(e.bg(), id)
		})
		n++
	}
	return n
}

// claimRedrive marks workerID as having an in-flight crash-recovery re-drive,
// returning false if one is already in flight (so the caller skips it).
func (e *Engine) claimRedrive(workerID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.redriving[workerID] {
		return false
	}
	e.redriving[workerID] = true
	return true
}

// releaseRedrive clears the in-flight marker once the re-drive completes (always
// runs via defer, even on a panic recovered by Exec).
func (e *Engine) releaseRedrive(workerID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.redriving, workerID)
}

// reapEscalations expires pending escalations older than EscalationTimeout and
// pauses the waiting worker (build-guide "timeout → auto-pause"), so a worker
// can't wait on a human indefinitely. Per-worker tx; a non-waiting worker (e.g. a
// danger escalation on an already-paused worker) is just expired + left as-is
// (still fail-safe). Returns how many were timed out.
func (e *Engine) reapEscalations(ctx context.Context) int {
	pend, err := e.Store.Reader().ListEscalations(core.EscalationFilter{Status: "pending"})
	if err != nil {
		return 0
	}
	cutoff := e.Store.Now().Add(-e.EscalationTimeout)
	n := 0
	for _, esc := range pend {
		ts, perr := time.Parse(time.RFC3339Nano, esc.RequestedAt)
		if perr != nil || !ts.Before(cutoff) {
			continue // unparseable or not yet timed out
		}
		var expired int
		txErr := e.Store.WithTx(ctx, func(tx core.Tx) error {
			var err error
			if expired, err = tx.ExpirePendingForWorker(esc.WorkerID); err != nil {
				return err
			}
			w, err := tx.GetWorker(esc.WorkerID)
			if err != nil {
				return nil
			}
			if isWaiting(w.State) && core.LegalWorkerTransition(w.State, core.WorkerPaused) {
				return tx.TransitionWorker(esc.WorkerID, core.WorkerPaused, w.Rev, core.Event{
					Kind: "state_change", WorkerID: esc.WorkerID, SessionID: w.OwnerSession,
					Payload: `{"reason":"escalation_timeout"}`,
				})
			}
			return nil
		})
		// POST-COMMIT: one warn card per escalation the operator never answered.
		if txErr == nil && expired > 0 {
			e.notifyCard(esc.SessionID, notify.Card{
				Level: notify.LevelWarn,
				Title: "arco: escalation expired — " + esc.WorkerID,
				Body:  fmt.Sprintf("worker: %s\nquestion: %s", esc.WorkerID, esc.Action),
			})
		}
		n++
	}
	return n
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

// aliveLookup indexes the alive agents by both their backend ref (herdr pane_id)
// and their workspace, and returns a per-worker liveness lookup that prefers the
// worker's AgentRef (set once launched via the arco-owned spawn path) and falls
// back to a Workspace match (Prompt-model / Fake workers with no ref yet).
func aliveLookup(agents []core.AgentObs) func(core.Worker) (core.AgentObs, bool) {
	byWS := map[string]core.AgentObs{}
	byRef := map[string]core.AgentObs{}
	for _, a := range agents {
		if !a.Alive {
			continue
		}
		if a.Workspace != "" {
			byWS[a.Workspace] = a
		}
		if a.Ref != "" {
			byRef[a.Ref] = a
		}
	}
	return func(w core.Worker) (core.AgentObs, bool) {
		if w.AgentRef != "" {
			obs, ok := byRef[w.AgentRef]
			return obs, ok
		}
		obs, ok := byWS[w.Workspace]
		return obs, ok
	}
}

// agentReclaimable reports whether a worker's herdr agent should no longer be
// running, so the sweep may stop it (reaper) and must not treat its absence as a
// death (liveness):
//   - TERMINAL (killed/failed/lost/completed_verified): done forever.
//   - PAUSED with NO pending escalation: intentionally stopped (pool-TTL /
//     escalation-timeout), its worktree preserved and resume via RELAUNCH, so its
//     idle agent is pure quota leak (MED-3 auto-kill-on-pause).
//
// A worker with a PENDING escalation is NOT reclaimable even when paused: an
// operator approval re-prompts the SAME live pane (deliverDecision reconnect —
// e.g. an AuditDeniedAttempt danger confirm), so its agent must survive to be
// re-driven. (A terminal worker is always reclaimable — decide() refuses to resume
// a terminal worker, so no live escalation can reconnect it.)
//
// INVARIANT for future changes: the pending-escalation check stands in for "no
// path will re-prompt THIS worker's live pane." A pending escalation is the only
// such path today (deliverDecision). If a new same-pane delivery is added (e.g.
// operator-to-worker messages, or task delivery reusing the live pane instead of
// relaunch), it MUST also keep the agent alive here (qwen review).
func agentReclaimable(w core.Worker, pendingEsc map[string]bool) bool {
	if w.State.Terminal() {
		return true
	}
	return w.State == core.WorkerPaused && !pendingEsc[w.ID]
}

// reapOrphanedAgents stops herdr agents still alive on a worker whose agent should
// no longer run (agentReclaimable: terminal OR paused). A lingering agent is pure
// quota leak — a crash between KillWorker's commit and its best-effort VM.Kill, a
// worker finalized lost/failed with a lingering pane, or a worker just paused by
// pool-TTL / escalation-timeout. Best-effort + idempotent: a Kill error leaves the
// agent listed to be re-reaped next sweep; a successful close removes it from
// ListAgents so it isn't targeted again. Returns how many were stopped.
//
// Reaping requires a POSITIVE identity match: the worker's recorded terminal_id
// (herdr's stable per-pane id, carried in the BootID slot) must equal the live
// agent's. This is the INVERSE posture of the liveness loop, which believes the
// ref unless identity is known-mismatched — because closing a workspace is
// DESTRUCTIVE, so on any identity doubt we must NOT act. Concretely: if our agent
// died and herdr recycled its pane_id/workspace_id to an unrelated live agent, a
// loose "believe the ref" reaper would wrongly close that innocent workspace
// (opus review). A worker terminalized/paused before ANY sweep observed it alive
// has no recorded terminal_id (persisted only on the liveness alive-path, not at
// launch) → it is left for manual cleanup: a rare, NON-destructive miss is the
// right trade against a destructive false-close (docs §12).
// vmc is the client of the VM the given workers/agents belong to — a kill must
// land on the host whose pane it is (T3.3).
func (e *Engine) reapOrphanedAgents(ctx context.Context, vmc core.VMClient, all []core.Worker, agents []core.AgentObs, pendingEsc map[string]bool) int {
	lookup := aliveLookup(agents)
	n := 0
	for _, w := range all {
		if !agentReclaimable(w, pendingEsc) {
			continue
		}
		obs, alive := lookup(w)
		if !alive {
			continue
		}
		// D9 gate: in a session whose mode forbids autonomous reclaim (manual —
		// arco must not touch the world), leave the agent running. The worker
		// stays listed; it is reaped if the operator flips the mode later.
		if !e.sessionMode(w.OwnerSession).Allows(core.ActReapAgent) {
			continue
		}
		// Positive-identity gate (see doc): reap only when we can confirm the live
		// agent on this ref is the SAME process we launched, never on a doubt.
		if w.BootID == "" || obs.BootID == "" || obs.BootID != w.BootID {
			continue
		}
		// Kill addresses a pane (herdr `workspace close` derives the ws from the
		// pane_id); a bare workspace label is not a valid target, so skip if we have
		// no captured ref (Fake/legacy prompt-model workers never capture one).
		ref := w.AgentRef
		if ref == "" {
			ref = obs.Ref
		}
		if ref == "" {
			continue
		}
		if err := vmc.Kill(ctx, ref); err == nil {
			n++
		}
	}
	return n
}

// triggerRollups enqueues a coalesced rollup for each ALIVE parent worker that
// has at least one terminal child. Returns how many parents were enqueued (the
// rollup itself coalesces to ≤1 brain call per session per interval).
func (e *Engine) triggerRollups(all []core.Worker) int {
	terminalParents := map[string]bool{}
	eligible := map[string]bool{}
	for _, w := range all {
		// Only enqueue a rollup for a parent the brain may actually drive — mirror
		// rollup()'s own guard so an ineligible parent (blocked/pool-owned) isn't
		// counted or submitted (capstone audit; rollup() re-checks in-tx too).
		if rollupEligible(w) {
			eligible[w.ID] = true
		}
		if w.ParentWorkerID != "" && w.State.Terminal() {
			terminalParents[w.ParentWorkerID] = true
		}
	}
	n := 0
	for pid := range terminalParents {
		if eligible[pid] {
			e.maybeRollup(pid)
			n++
		}
	}
	return n
}

// reapPooled pauses workers that have sat unclaimed in the pool past PoolTTL.
func (e *Engine) reapPooled(ctx context.Context) (int, error) {
	var n int
	err := e.Store.WithTx(ctx, func(tx core.Tx) error {
		var err error
		n, err = tx.ReapPooledWorkers(e.PoolTTL)
		return err
	})
	return n, err
}

func (e *Engine) finalize(ctx context.Context, w core.Worker, target core.WorkerState, headNow string) (bool, error) {
	var transitioned bool // the worker was actually moved to the target in the tx
	err := e.Store.WithTx(ctx, func(tx core.Tx) error {
		transitioned = false
		cur, err := tx.GetWorker(w.ID)
		if err != nil {
			return err
		}
		if cur.State.Terminal() {
			return nil
		}
		// The HEAD-advanced target is completed_candidate, but that's illegal from
		// blocked/paused/waiting_* — fall back to `lost` (legal from every
		// non-terminal state) so a dead worker in one of those states TERMINALIZES
		// instead of wedging forever (finalize would no-op every sweep, and its
		// misses entry would never be reset → an unbounded map leak). Capstone audit.
		if !core.LegalWorkerTransition(cur.State, target) {
			if !core.LegalWorkerTransition(cur.State, core.WorkerLost) {
				return nil
			}
			target = core.WorkerLost
		}
		if headNow != "" {
			if err := tx.ObserveWorker(w.ID, core.WorkerObservation{HeadCommit: headNow}); err != nil {
				return err
			}
		}
		if err := tx.TransitionWorker(w.ID, target, cur.Rev, core.Event{
			Kind: "reconcile", WorkerID: w.ID, SessionID: cur.OwnerSession,
			Payload: fmt.Sprintf(`{"sweep":true,"target":%q}`, target),
		}); err != nil {
			return err
		}
		transitioned = true
		// Expire any pending escalation as the worker terminalizes, so a later human
		// answer can't drive lost→running and resurrect a dead worker (capstone audit).
		_, xerr := tx.ExpirePendingForWorker(w.ID)
		return xerr
	})
	if errors.Is(err, core.ErrRevMismatch) {
		return false, nil
	}
	// POST-COMMIT: a worker finalized `lost` (missed too many sweeps with no HEAD
	// progress) is worth paging about; completed_candidate is not (it made progress).
	if err == nil && transitioned && target == core.WorkerLost {
		e.notifyCard(w.OwnerSession, notify.Card{
			Level: notify.LevelWarn,
			Title: "arco: worker lost — " + w.ID,
			Body:  fmt.Sprintf("worker: %s\nagent missing for %d consecutive sweeps", w.ID, e.MissThreshold),
		})
	}
	return err == nil, err
}

// checkStall enforces StallN for one ALIVE worker (called after its observation
// is recorded): a running worker whose git HEAD was observed and did NOT advance
// is making no progress. After StallN consecutive such sweeps it is transitioned
// running→blocked and a stall question escalation is opened in ONE tx
// (OpenEscalation keeps it idempotent one-pending-per-worker), and the counter
// is reset in the same tx so a later unblock starts fresh. HEAD advance resets
// the counter; an unobservable HEAD is no signal at all (absence of evidence is
// not no-progress). Only state `running` accrues/triggers stall. Returns whether
// the worker was blocked.
func (e *Engine) checkStall(ctx context.Context, w core.Worker, headNow string) (bool, error) {
	if e.StallN <= 0 || w.State != core.WorkerRunning || headNow == "" {
		return false, nil
	}
	if headNow != w.HeadCommit {
		if w.StallCount == 0 {
			return false, nil // already zero — no write needed
		}
		err := e.Store.WithTx(ctx, func(tx core.Tx) error {
			return tx.ResetWorkerStall(w.ID)
		})
		return false, err
	}
	var blocked bool
	var opened bool // the stall question was NEWLY opened (no dedup) → notify
	var action, task, sess string
	err := e.Store.WithTx(ctx, func(tx core.Tx) error {
		cur, err := tx.GetWorker(w.ID)
		if err != nil {
			return err
		}
		if cur.State != core.WorkerRunning {
			return nil // another writer moved it; the next sweep re-derives
		}
		n, err := tx.BumpWorkerStall(w.ID)
		if err != nil {
			return err
		}
		if n < e.StallN {
			return nil
		}
		if err := tx.TransitionWorker(w.ID, core.WorkerBlocked, cur.Rev, core.Event{
			Kind: "reconcile", WorkerID: w.ID, SessionID: cur.OwnerSession,
			Payload: fmt.Sprintf(`{"stall":true,"sweeps":%d}`, n),
		}); err != nil {
			return err
		}
		// QuestionClass must stay inside the frozen schema's enum (0001 CHECK):
		// the catch-all "other" carries it, the stall semantics live in Action +
		// the reconcile event payload ("stall":true). A dedicated 'stall' class
		// would need an escalations-table rebuild — out of scope (no migrations).
		action = fmt.Sprintf("worker made no progress for %d sweeps", n)
		// Detect NEWLY-opened for the notify card (a dedup must not re-notify);
		// reading the pending state in the SAME serialized tx is race-free.
		pend, err := tx.ListEscalations(core.EscalationFilter{Status: "pending", WorkerID: w.ID})
		if err != nil {
			return err
		}
		if _, err := tx.OpenEscalation(core.Escalation{
			WorkerID: w.ID, SessionID: cur.OwnerSession, Kind: "question",
			QuestionClass: "other", ActionClass: core.ClassAmbiguous, Tier: core.TierMedium,
			Action: action,
		}); err != nil {
			return err
		}
		blocked = true
		if len(pend) == 0 {
			opened, task, sess = true, cur.Task, cur.OwnerSession
		}
		return tx.ResetWorkerStall(w.ID) // same tx: a later unblock starts fresh
	})
	if err != nil {
		return false, err
	}
	// POST-COMMIT: push the decision card for a newly-opened stall question.
	if opened {
		card := notify.EscalationCard{
			WorkerID: w.ID,
			TaskTail: taskTail(task),
			Question: action,
		}
		if esc, ok := e.pendingEscForCard(w.ID); ok {
			card.EscalationID, card.Kind, card.SessionID = esc.ID, esc.Kind, sess
		}
		e.notifyCard(sess, notify.FormatEscalation(card))
	}
	return blocked, nil
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
	// Per-VM liveness lookups (T3.3): each starting worker is resolved against
	// ITS OWN VM's agent list, fetched once per VM. nil lookup = that VM is
	// unobservable this boot (registry gap, or — routing on — a ListAgents
	// error): leave its workers `starting` for a later sweep rather than parking
	// a possibly-live agent's worker failed on transport noise. Routing off
	// keeps today's posture — the single client's ListAgents error aborts.
	lookups := map[string]func(core.Worker) (core.AgentObs, bool){}
	lookupFor := func(vmName string) (func(core.Worker) (core.AgentObs, bool), error) {
		key := ""
		if e.VMs != nil {
			key = vmName
		}
		if lu, ok := lookups[key]; ok {
			return lu, nil
		}
		var lu func(core.Worker) (core.AgentObs, bool)
		if c, cerr := e.vmFor(key); cerr == nil {
			if agents, aerr := c.ListAgents(ctx); aerr == nil {
				lu = aliveLookup(agents)
			} else if e.VMs == nil {
				return nil, aerr
			}
		}
		lookups[key] = lu
		return lu, nil
	}
	var firstErr error
	for _, w := range starting {
		lookupAlive, lerr := lookupFor(w.VM)
		if lerr != nil {
			return lerr
		}
		if lookupAlive == nil {
			continue // unobservable VM: resolved by a later sweep, never a blind park
		}
		obs, alive := lookupAlive(w)
		// same PID-reuse identity guard as Sweep: don't adopt a recycled workspace.
		if alive && w.BootID != "" && obs.BootID != "" && obs.BootID != w.BootID {
			alive = false
		}
		target := core.WorkerFailed
		reason := "boot: dispatch_intent without dispatch_done, process not found"
		if alive {
			target, reason = core.WorkerRunning, "boot: adopted live worker"
		}
		var transitioned bool
		if err := e.Store.WithTx(ctx, func(tx core.Tx) error {
			transitioned = false
			cur, err := tx.GetWorker(w.ID)
			if err != nil {
				return err
			}
			if cur.State != core.WorkerStarting {
				return nil
			}
			if err := tx.TransitionWorker(w.ID, target, cur.Rev, core.Event{
				Kind: "reconcile", WorkerID: w.ID, SessionID: cur.OwnerSession,
				Payload: fmt.Sprintf(`{"boot_recovery":true,"reason":%q}`, reason),
			}); err != nil {
				return err
			}
			transitioned = true
			return nil
		}); err != nil && firstErr == nil {
			firstErr = err // record but keep going: one bad worker mustn't skip the rest + the sweep
		}
		// POST-COMMIT: a launch that never came up (parked failed at boot) surfaces.
		if transitioned && target == core.WorkerFailed {
			e.notifyCard(w.OwnerSession, notify.Card{
				Level: notify.LevelWarn,
				Title: "arco: worker failed — " + w.ID,
				Body:  fmt.Sprintf("worker: %s\n%s", w.ID, reason),
			})
		}
	}
	// Always run the sweep, even if a per-worker recovery failed, so live-but-
	// dead `running` workers are repaired.
	if _, err := e.Sweep(ctx); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}
