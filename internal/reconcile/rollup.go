package reconcile

import (
	"context"
	"fmt"
	"strings"

	"github.com/dinhlongviolin1/arco/internal/brain"
	"github.com/dinhlongviolin1/arco/internal/core"
)

// Supersession rollup (build-guide-rev6 PASS-3, plan Decision B). When a
// delegated child completes, its parent ("root") is re-driven by a short-lived
// brain call that summarizes the children's results — but COALESCED to at most
// one rollup per session per RollupInterval (a 100-child root would otherwise
// storm the brain). The rollup summary is ADVISORY (M19 injection closure): it
// is marked tainted + call_kind=rollup for provenance, and — like every brain
// StepResult — can never promote a grant or decide a confirm (those require a
// human via the escalation decide path), so a child result can't launder
// authority into the parent's tree.

// maybeRollup enqueues a coalesced rollup for a completed child's parent, OFF
// the write path (serialized per parent via Exec). No-op when rollup or the
// brain is disabled, or the child has no parent.
func (e *Engine) maybeRollup(parentWorkerID string) {
	if parentWorkerID == "" || e.RollupInterval <= 0 || !e.Brain.Enabled || e.Exec == nil {
		return
	}
	pid := parentWorkerID
	e.Exec.Submit(pid, func() { e.rollup(e.bg(), pid) })
}

// rollupEligible gates the brain from a rollup exactly as ApplyEvent
// (engine.go) and brainClassify (brain_apply.go) gate it — rollup is the THIRD
// brain-entry path, and the per-PR reviews of those two guards missed it
// (capstone audit). A rollup must NOT fire on:
//   - a terminal worker (nothing to drive);
//   - a BLOCKED worker (parked, e.g. by a billing wall) — else question/confirm
//     would un-park it and dispatch would spawn from a parked parent;
//   - a POOL-OWNED worker (handoff-released sentinel) — else it runs a paid brain
//     loop on the protected pool, prompts/pulls-out an unowned worker, and opens
//     escalations attributed to the pool session;
//   - a WAITING or PAUSED worker — brainClassify only ever classifies
//     running|starting, and applyStep's acting branches (dispatch/handoff) assume
//     that; a rollup on a waiting_for_user/paused parent would un-park it
//     (candidate/waiting→running is legal) and strand its pending escalation, or
//     spawn a child from a parked parent (rev20 review #07/#2). So gate to the
//     same running|starting set the other two brain entries use.
func rollupEligible(w core.Worker) bool {
	return (w.State == core.WorkerRunning || w.State == core.WorkerStarting) &&
		w.OwnerSession != core.PoolSessionID
}

// rollup runs one coalesced rollup brain call for a parent worker. The coalesce
// check + rollup_intent record happen in one write tx under the single-writer
// lock, so concurrent child completions in a session yield at most one rollup
// per interval (race-free, mirrors the brain-rate gate).
func (e *Engine) rollup(ctx context.Context, parentWorkerID string) {
	// Estop, checked at EXECUTION time (mirrors brainClassify): the sweep gates
	// rollup submission on !paused, but a rollup queued on the per-parent Exec
	// before the operator paused could otherwise still fire a brain call and act.
	if e.Paused() {
		return
	}
	// Cheap pre-check to avoid a tx for a parent the brain must not touch.
	if w, err := e.Store.Reader().GetWorker(parentWorkerID); err != nil || !rollupEligible(w) {
		return
	}

	proceed := true
	var parent core.Worker
	_ = e.Store.WithTx(ctx, func(tx core.Tx) error {
		// Re-read INSIDE the tx: a concurrent TransferWorker moves a running worker
		// between sessions, so the pre-check snapshot's OwnerSession/state can be
		// stale — attribute the rollup to the CURRENT owner and re-check terminal
		// (mirrors brainClassify; opus review).
		cur, err := tx.GetWorker(parentWorkerID)
		if err != nil || !rollupEligible(cur) {
			proceed = false
			return nil
		}
		// D9 mode gate on the CURRENT owner: a rollup is a brain draft, so a
		// manual session (or unknown mode) must get none — and, like brainClassify,
		// return BEFORE recording rollup_intent so the ledger carries no trace of a
		// call that must not happen (rev20 review #07/#1).
		s, serr := tx.GetSession(cur.OwnerSession)
		if serr != nil {
			proceed = false
			return nil
		}
		if mode, merr := core.ParseSupervisionMode(string(s.SupervisionMode)); merr != nil || !mode.Allows(core.ActBrainDraft) {
			proceed = false
			return nil
		}
		parent = cur
		n, err := tx.CountRecentRollups(parentWorkerID, e.RollupInterval) // per-parent
		if err != nil {
			return err
		}
		if n >= 1 { // already rolled up this interval → coalesce (skip)
			proceed = false
			return nil
		}
		_, _, _, e2 := tx.AppendEvent(core.Event{
			Kind: "rollup_intent", WorkerID: parentWorkerID, SessionID: cur.OwnerSession, Actor: "brain",
			// M19 provenance: rollup context is advisory + tainted, never authority.
			Payload: `{"call_kind":"rollup","tainted":true}`,
		})
		return e2
	})
	if !proceed {
		return
	}

	// Assemble the rollup context from the parent's TERMINAL children (their
	// results), then re-drive the parent via the brain. Reads best-effort.
	children, _ := e.terminalChildren(parent)
	sess, _ := e.Store.Reader().GetSession(parent.OwnerSession)
	prompt := assembleRollupContext(parent, sess, children)
	if e.Redact != nil {
		prompt, _ = e.Redact.Scrub(prompt)
	}
	res := brain.Invoke(ctx, brain.Config{Profile: e.Brain.Profile, Model: e.Brain.Model}, prompt, e.Brain.Runner)
	e.meterBrainCall(approxTokens(prompt, res.Raw))
	if res.Billing || res.Malformed || res.Err != nil {
		// A bad rollup call is advisory noise — record it, don't park the parent
		// (the parent isn't blocked on the rollup; it's a periodic review).
		e.errorEvent(ctx, parentWorkerID, "rollup brain call unusable")
		return
	}
	// Empty cid: the rollup path records a rollup_intent, not a brain_intent, so it
	// is outside the stale-brain-intent re-drive (its own interval-coalescing
	// bounds a lost rollup call); no side-effect correlation is needed here.
	e.applyStep(ctx, parentWorkerID, "", res.Step)
}

// terminalChildren returns the parent's completed/failed child workers (the
// results a rollup summarizes).
func (e *Engine) terminalChildren(parent core.Worker) ([]core.Worker, error) {
	siblings, err := e.Store.Reader().ListWorkers(core.WorkerFilter{OwnerSession: parent.OwnerSession})
	if err != nil {
		return nil, err
	}
	var out []core.Worker
	for _, w := range siblings {
		if w.ParentWorkerID == parent.ID && w.State.Terminal() {
			out = append(out, w)
		}
	}
	return out, nil
}

// assembleRollupContext is a byte-stable rollup decision prompt: the parent, its
// session goal, and each terminal child's outcome (deterministic — ListWorkers
// orders by id, no clock/map). Child Summary/Task are already write-scrubbed.
func assembleRollupContext(parent core.Worker, s core.Session, children []core.Worker) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Rollup for parent worker %s state=%s task=%q\n", parent.ID, parent.State, truncate(parent.Task, fieldCap))
	if s.Goal != "" {
		fmt.Fprintf(&b, "Session goal: %s\n", truncate(s.Goal, fieldCap))
	}
	fmt.Fprintf(&b, "Completed sub-workers (%d):\n", len(children))
	for _, c := range children {
		fmt.Fprintf(&b, "  - %s state=%s task=%q summary=%q\n",
			c.ID, c.State, truncate(c.Task, eventPayloadCap), truncate(c.Summary, eventPayloadCap))
	}
	b.WriteString("Given these sub-worker results, decide the parent's next step and reply with a JSON StepResult " +
		`{"kind":"run_again|dispatch|handoff|final_output|question|confirm","instruction":"...","reason":"..."}.`)
	return b.String()
}
