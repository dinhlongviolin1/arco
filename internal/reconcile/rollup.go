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

// rollup runs one coalesced rollup brain call for a parent worker. The coalesce
// check + rollup_intent record happen in one write tx under the single-writer
// lock, so concurrent child completions in a session yield at most one rollup
// per interval (race-free, mirrors the brain-rate gate).
func (e *Engine) rollup(ctx context.Context, parentWorkerID string) {
	// Cheap pre-check to avoid a tx for an obviously-dead parent.
	if w, err := e.Store.Reader().GetWorker(parentWorkerID); err != nil || w.State.Terminal() {
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
		if err != nil || cur.State.Terminal() {
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
	if res.Billing || res.Malformed || res.Err != nil {
		// A bad rollup call is advisory noise — record it, don't park the parent
		// (the parent isn't blocked on the rollup; it's a periodic review).
		e.errorEvent(ctx, parentWorkerID, "rollup brain call unusable")
		return
	}
	e.applyStep(ctx, parentWorkerID, res.Step)
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
