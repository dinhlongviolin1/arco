package reconcile

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/oklog/ulid/v2"

	"github.com/dinhlongviolin1/arco/internal/brain"
	"github.com/dinhlongviolin1/arco/internal/core"
)

// brainClassify runs a short-lived brain call for an ambiguous worker OFF the
// write path (it executes inside an Exec per-worker goroutine, never inside a
// ledger tx), then applies the typed StepResult. Malformed/billing/errors park
// the worker `blocked` — never a crash-loop, never a retry into a billing wall.
func (e *Engine) brainClassify(ctx context.Context, workerID string) {
	w, err := e.Store.Reader().GetWorker(workerID)
	// Only a still-running/starting worker is brain-classifiable. If a concurrent
	// intake already resolved it (waiting/blocked/candidate/terminal), skip — no
	// redundant clavis shell, no stale StepResult (qwen #5).
	if err != nil || (w.State != core.WorkerRunning && w.State != core.WorkerStarting) {
		return
	}
	// A pool-owned (handoff-released, unclaimed) worker is inert to the brain —
	// chokepoint belt for the ApplyEvent gate (covers any other brainClassify caller).
	if w.OwnerSession == core.PoolSessionID {
		return
	}
	// cid correlates this classification: the brain_intent carries it, and so does
	// whichever un-recallable side effect the decision fires (prompt_intent for
	// run_again, brain_dispatch for dispatch — the only two decisions that leave
	// the worker RUNNING). The sweep re-drives a worker whose most-recent
	// brain_intent has NO event sharing its cid (a call lost to a crash BEFORE it
	// acted); a fired side effect leaves a cid-sibling, so a re-drive can never
	// duplicate it (see Store.StaleBrainIntents + Sweep).
	cid := ulid.Make().String()

	// Per-session brain-rate admission + persist brain INTENT, in ONE write tx so
	// the count→admit→insert is race-free under the single-writer lock (a burst of
	// ambiguous workers in a session can't slip past the cap). Over the cap: record
	// brain_rate_limited and skip the call — the worker stays as-is and is
	// re-evaluated on its next ambiguous signal (no park, no retry storm). A
	// rate-limited attempt carries NO cid, so it never resolves the dangling
	// brain_intent — a later, un-throttled sweep still re-drives it.
	proceed := true
	_ = e.Store.WithTx(ctx, func(tx core.Tx) error {
		// Re-read the worker INSIDE the tx so the rate count + intent are attributed
		// to its CURRENT owner — a concurrent ownership transfer between the read
		// above and here must not mis-account this call to the prior session.
		cur, err := tx.GetWorker(workerID)
		if err != nil {
			proceed = false
			return nil
		}
		if e.BrainRate > 0 {
			n, err := tx.CountRecentBrainCalls(cur.OwnerSession, time.Minute)
			if err != nil {
				return err
			}
			if n >= e.BrainRate {
				proceed = false
				_, _, _, e2 := tx.AppendEvent(core.Event{
					Kind: "brain_rate_limited", WorkerID: workerID, SessionID: cur.OwnerSession, Actor: "brain",
					Payload: fmt.Sprintf(`{"limit":%d,"window":"1m"}`, e.BrainRate),
				})
				return e2
			}
		}
		_, _, _, e2 := tx.AppendEvent(core.Event{
			Kind: "brain_intent", WorkerID: workerID, SessionID: cur.OwnerSession, Actor: "brain",
			CorrelationID: cid, Payload: fmt.Sprintf(`{"state":%q}`, cur.State),
		})
		return e2
	})
	if !proceed {
		return
	}

	// Assemble decision context: worker + its owning session goal + a bounded,
	// chronological event tail. Reads are best-effort — a read error just yields a
	// thinner prompt, never a failed classification.
	sess, _ := e.Store.Reader().GetSession(w.OwnerSession)
	tail, _ := e.Store.Reader().RecentWorkerEvents(w.ID, contextEventTail)
	prompt := assembleContext(w, sess, tail)
	if e.Redact != nil { // scrub BEFORE the prompt leaves for a third-party LLM
		prompt, _ = e.Redact.Scrub(prompt)
	}
	res := brain.Invoke(ctx, brain.Config{Profile: e.Brain.Profile, Model: e.Brain.Model},
		prompt, e.Brain.Runner)

	switch {
	case res.Billing:
		e.park(ctx, workerID, "brain billing wall — parked, not retried")
	case res.Malformed || res.Err != nil:
		e.park(ctx, workerID, "brain output unusable — parked")
	default:
		e.applyStep(ctx, workerID, cid, res.Step)
	}
	// Resolve the cid unconditionally once the classification has been PROCESSED
	// (parked or applied), so a re-drive fires ONLY for a call lost mid-flight.
	// The two un-recallable side effects already stamp cid on their own intent
	// (prompt_intent / brain_dispatch) BEFORE acting — this covers every OTHER
	// outcome that leaves the worker running without a side effect: a denied/failed
	// delegation, an unhandled kind, or a decision whose transition is a legal
	// no-op (e.g. final_output on a still-`starting` worker). Without it those loop
	// on the brain every grace interval forever (opus review).
	e.markBrainResolved(ctx, workerID, cid)
}

// markBrainResolved appends a cid-stamped brain_resolved event so the sweep's
// stale-brain-intent detection treats the classification as durably processed.
// Best-effort: if it fails, the worst case is one extra (safe, idempotent)
// re-drive of a no-side-effect outcome — a fired side effect is already guarded
// by its own cid-stamped intent, and the rate-limited path (which returns before
// this) intentionally leaves the cid unresolved so a later sweep retries.
func (e *Engine) markBrainResolved(ctx context.Context, workerID, cid string) {
	_ = e.Store.WithTx(ctx, func(tx core.Tx) error {
		w, err := tx.GetWorker(workerID)
		if err != nil {
			return nil
		}
		_, _, _, e2 := tx.AppendEvent(core.Event{
			Kind: "brain_resolved", WorkerID: workerID, SessionID: w.OwnerSession, Actor: "brain",
			CorrelationID: cid, Payload: "{}",
		})
		return e2
	})
}

const (
	contextEventTail = 20   // how many recent events feed the decision prompt
	eventPayloadCap  = 200  // per-event payload truncation (bounds prompt size)
	fieldCap         = 2000 // per free-text field (task/goal) cap (bounds prompt size)
)

// assembleContext builds the side-effect-free decision prompt from the worker,
// its owning session, and a chronological event tail. It is BYTE-STABLE: given
// the same inputs it produces identical bytes (no clock reads, no map iteration,
// events already ordered by id) so prompt_hash telemetry is deterministic over
// the post-Scrub bytes (rev-4.1 #5). Payloads are already write-time scrubbed
// (B4); e.Redact re-scrubs the whole prompt as belt-and-suspenders before it
// leaves for the third-party LLM.
func assembleContext(w core.Worker, s core.Session, events []core.Event) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Worker %s state=%s task=%q", w.ID, w.State, truncate(w.Task, fieldCap))
	if w.DelegationDepth > 0 {
		fmt.Fprintf(&b, " depth=%d parent=%s", w.DelegationDepth, w.ParentWorkerID)
	}
	b.WriteByte('\n')
	if s.Goal != "" {
		fmt.Fprintf(&b, "Session goal: %s\n", truncate(s.Goal, fieldCap))
	}
	if len(events) > 0 {
		b.WriteString("Recent events (oldest→newest):\n")
		for _, ev := range events {
			fmt.Fprintf(&b, "  [%d] %s %s\n", ev.ID, ev.Kind, truncate(ev.Payload, eventPayloadCap))
		}
	}
	b.WriteString("Decide the next step and reply with a JSON StepResult " +
		`{"kind":"run_again|dispatch|handoff|final_output|question|confirm","instruction":"...","reason":"..."}.`)
	return b.String()
}

// truncate bounds a payload to ~n bytes with an explicit marker (byte-stable).
// It backs off to a UTF-8 rune boundary so a multi-byte rune isn't split.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…(truncated)"
}

// applyStep reconciles a StepResult in a fresh tx (re-validating rev). Every
// StepResult.Kind has a branch; an unhandled kind is an error event, not a
// silent drop.
// applyStep applies a brain StepResult. cid is the classification's correlation
// id (empty for the rollup path, which records no brain_intent): the two
// decisions that leave the worker RUNNING with an un-recallable side effect —
// run_again (prompt) and dispatch (child) — stamp cid on their side-effect
// intent so a crash-recovery re-drive can tell "already acted" from "lost".
func (e *Engine) applyStep(ctx context.Context, workerID, cid string, step core.StepResult) {
	// run_again/dispatch prompt the worker — a side effect done BEFORE the tx so
	// a tx never holds while shelling out.
	switch step.Kind {
	case "run_again":
		// Re-read + record prompt_intent (stamped with cid) under the write lock
		// RIGHT before the prompt, and skip if the worker has since moved (a
		// concurrent intake transition) — the CAS protects state, prompt_intent(cid)
		// protects the un-recallable external side effect from a re-drive.
		ws, ok := e.preparePrompt(ctx, workerID, cid, step)
		if !ok {
			return
		}
		if err := e.VM.Prompt(ctx, ws, promptIntentText(step.Instruction)); err != nil {
			// Delivery failed — do NOT record a normal running decision (the ledger
			// would claim running while the worker was never prompted). Park it.
			e.park(ctx, workerID, "brain prompt delivery failed: "+err.Error())
			return
		}
		e.recordDecision(ctx, workerID, step, core.WorkerRunning)
	case "dispatch":
		// The brain delegates a subtask → spawn a CHILD worker (depth + per-session
		// fan-in gated). The parent stays running while the child works. A denied
		// delegation (depth/fan-in) is an audit event, never a crash or silent drop.
		// cid flows to Delegate, which records a parent-scoped brain_dispatch(cid)
		// ATOMICALLY in the child-create tx: child-exists ⟹ cid resolved ⟹ a
		// re-drive can't spawn a duplicate child.
		if _, err := e.Delegate(ctx, workerID, step.Instruction, cid); err != nil {
			switch {
			case errors.Is(err, core.ErrMaxDepthExceeded), errors.Is(err, core.ErrFanInExceeded):
				e.errorEvent(ctx, workerID, "delegation denied: "+err.Error())
			default:
				e.errorEvent(ctx, workerID, "delegation failed: "+err.Error())
			}
			return
		}
		e.recordDecision(ctx, workerID, step, core.WorkerRunning)
	case "final_output":
		e.transitionFromBrain(ctx, workerID, core.WorkerCompletedCandidate, step)
	case "question":
		e.openFromBrain(ctx, workerID, "question", core.ClassAmbiguous, core.TierMedium, step)
	case "confirm":
		e.openFromBrain(ctx, workerID, "confirm", core.ClassDanger, core.TierHighBlast, step)
	case "handoff":
		// The worker hands ownership back to arco → release it to the pool (PASS-3
		// ownership transfer). It stays running, unowned, until claimed or
		// pool-TTL-paused; ReleaseWorker emits the intent/released audit events.
		if err := e.Store.WithTx(ctx, func(tx core.Tx) error {
			return tx.ReleaseWorker(workerID, "brain")
		}); err != nil {
			e.errorEvent(ctx, workerID, "handoff release failed: "+err.Error())
		}
	default:
		e.errorEvent(ctx, workerID, "unhandled StepResult kind: "+step.Kind)
	}
}

// preparePrompt verifies (under the write lock) that the worker is still in a
// promptable state and records prompt_intent; returns the workspace + ok. If the
// worker moved, ok=false and the caller skips the prompt.
func (e *Engine) preparePrompt(ctx context.Context, workerID, cid string, step core.StepResult) (workspace string, ok bool) {
	_ = e.Store.WithTx(ctx, func(tx core.Tx) error {
		w, err := tx.GetWorker(workerID)
		if err != nil {
			return nil
		}
		if w.State != core.WorkerRunning && w.State != core.WorkerStarting {
			return nil // moved (e.g. now waiting/blocked/terminal) → skip the prompt
		}
		// Execute-time owner re-validation of the un-recallable prompt: a worker
		// RELEASED to the pool (via the transfer API, off the per-worker Exec queue)
		// between the brain's entry gate and here must not be prompted — the pool is
		// inert to the brain (completes the guard-set in ApplyEvent/brainClassify/
		// Delegate; opus capstone review). The prompt_intent's `owner_session` also
		// stays attributed to the CURRENT owner.
		if w.OwnerSession == core.PoolSessionID {
			return nil
		}
		// Target the worker's pane_id (AgentRef) so herdr `agent prompt` addresses
		// the right pane; falls back to the workspace label for the Fake/legacy
		// prompt-path (no captured pane). Returned to the caller as the Prompt target.
		workspace, ok = promptTarget(w), true
		_, _, _, e2 := tx.AppendEvent(core.Event{
			Kind: "prompt_intent", WorkerID: workerID, SessionID: w.OwnerSession, Actor: "brain",
			CorrelationID: cid, Payload: fmt.Sprintf(`{"instruction":%q}`, step.Instruction),
		})
		return e2
	})
	return
}

func (e *Engine) transitionFromBrain(ctx context.Context, workerID string, to core.WorkerState, step core.StepResult) {
	_ = e.Store.WithTx(ctx, func(tx core.Tx) error {
		w, err := tx.GetWorker(workerID)
		if err != nil || w.State.Terminal() || w.OwnerSession == core.PoolSessionID || !core.LegalWorkerTransition(w.State, to) {
			return nil // released to the pool mid-flight → inert to the brain (don't terminalize a pooled worker a claim may be racing)
		}
		return tx.TransitionWorker(workerID, to, w.Rev, core.Event{
			Kind: "brain_decision", WorkerID: workerID, SessionID: w.OwnerSession, Actor: "brain",
			Payload: fmt.Sprintf(`{"kind":%q,"reason":%q}`, step.Kind, step.Reason),
		})
	})
}

func (e *Engine) recordDecision(ctx context.Context, workerID string, step core.StepResult, ensure core.WorkerState) {
	_ = e.Store.WithTx(ctx, func(tx core.Tx) error {
		w, err := tx.GetWorker(workerID)
		if err != nil || w.State.Terminal() {
			return nil
		}
		if w.State != ensure && core.LegalWorkerTransition(w.State, ensure) {
			return tx.TransitionWorker(workerID, ensure, w.Rev, core.Event{
				Kind: "brain_decision", WorkerID: workerID, SessionID: w.OwnerSession, Actor: "brain",
				Payload: fmt.Sprintf(`{"kind":%q}`, step.Kind),
			})
		}
		_, _, _, e2 := tx.AppendEvent(core.Event{
			Kind: "brain_decision", WorkerID: workerID, SessionID: w.OwnerSession, Actor: "brain",
			Payload: fmt.Sprintf(`{"kind":%q}`, step.Kind),
		})
		return e2
	})
}

func (e *Engine) openFromBrain(ctx context.Context, workerID, kind string, ac core.ActionClass, tier core.Tier, step core.StepResult) {
	waiting := core.WorkerWaitingForUser
	if kind == "confirm" {
		waiting = core.WorkerWaitingConfirmation
	}
	_ = e.Store.WithTx(ctx, func(tx core.Tx) error {
		w, err := tx.GetWorker(workerID)
		if err != nil {
			return nil
		}
		// Released to the pool mid-flight → inert to the brain: opening an escalation
		// would attribute it to the pool session AND strand the pooled worker in a
		// waiting state (the same pool-attributed-escalation harm as the #44 rollup
		// finding). Skip.
		if w.OwnerSession == core.PoolSessionID {
			return nil
		}
		// If the worker moved and can't enter the waiting state, do NOT open a
		// stale escalation — a human answering it could resurrect a finished
		// worker (qwen #4). Only open when the transition actually applies.
		if w.State != waiting && !core.LegalWorkerTransition(w.State, waiting) {
			return nil
		}
		if w.State != waiting {
			if err := tx.TransitionWorker(workerID, waiting, w.Rev, core.Event{
				Kind: "brain_decision", WorkerID: workerID, SessionID: w.OwnerSession, Actor: "brain",
				Payload: fmt.Sprintf(`{"kind":%q}`, kind),
			}); err != nil {
				return err
			}
		}
		_, err = tx.OpenEscalation(core.Escalation{
			WorkerID: workerID, SessionID: w.OwnerSession, Kind: kind, ActionClass: ac, Tier: tier,
			QuestionClass: "clarify", Action: step.Instruction, DraftAnswer: step.Reason, BrainRationale: step.Reason,
		})
		return err
	})
}

func (e *Engine) park(ctx context.Context, workerID, reason string) {
	_ = e.Store.WithTx(ctx, func(tx core.Tx) error {
		w, err := tx.GetWorker(workerID)
		if err != nil || w.State.Terminal() || w.OwnerSession == core.PoolSessionID || !core.LegalWorkerTransition(w.State, core.WorkerBlocked) {
			return nil // released to the pool mid-flight → don't park a pooled worker a claim may be racing
		}
		return tx.TransitionWorker(workerID, core.WorkerBlocked, w.Rev, core.Event{
			Kind: "error", WorkerID: workerID, SessionID: w.OwnerSession, Actor: "brain",
			Payload: fmt.Sprintf(`{"parked":%q}`, reason),
		})
	})
}

func (e *Engine) errorEvent(ctx context.Context, workerID, msg string) {
	_ = e.Store.WithTx(ctx, func(tx core.Tx) error {
		w, err := tx.GetWorker(workerID)
		if err != nil {
			return nil
		}
		_, _, _, e2 := tx.AppendEvent(core.Event{
			Kind: "error", WorkerID: workerID, SessionID: w.OwnerSession, Actor: "brain",
			Payload: fmt.Sprintf(`{"error":%q}`, msg),
		})
		return e2
	})
}

// promptIntentText embeds an intent ULID so the normalizer can later prove
// delivery (build-guide B9 prompt_intent/prompt_done). Minimal form for now.
func promptIntentText(instruction string) string {
	return "[arco-intent] " + instruction
}
