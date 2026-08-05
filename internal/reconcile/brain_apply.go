package reconcile

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

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
	// Per-session brain-rate admission + persist brain INTENT, in ONE write tx so
	// the count→admit→insert is race-free under the single-writer lock (a burst of
	// ambiguous workers in a session can't slip past the cap). Over the cap: record
	// brain_rate_limited and skip the call — the worker stays as-is and is
	// re-evaluated on its next ambiguous signal (no park, no retry storm). NOTE:
	// full crash re-drive of a dangling brain_intent is a later pass.
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
			Payload: fmt.Sprintf(`{"state":%q}`, cur.State),
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

	if res.Billing {
		e.park(ctx, workerID, "brain billing wall — parked, not retried")
		return
	}
	if res.Malformed || res.Err != nil {
		e.park(ctx, workerID, "brain output unusable — parked")
		return
	}
	e.applyStep(ctx, workerID, res.Step)
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
func (e *Engine) applyStep(ctx context.Context, workerID string, step core.StepResult) {
	// run_again/dispatch prompt the worker — a side effect done BEFORE the tx so
	// a tx never holds while shelling out.
	switch step.Kind {
	case "run_again":
		// Re-read + record prompt_intent under the write lock RIGHT before the
		// prompt, and skip if the worker has since moved (a concurrent intake
		// transition) — the CAS protects state, this protects the un-recallable
		// external side effect.
		ws, ok := e.preparePrompt(ctx, workerID, step)
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
		if _, err := e.Delegate(ctx, workerID, step.Instruction); err != nil {
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
		// PASS-3 feature; reject in P2 with an audit trail (never silently drop).
		e.errorEvent(ctx, workerID, "handoff not supported in P2")
	default:
		e.errorEvent(ctx, workerID, "unhandled StepResult kind: "+step.Kind)
	}
}

// preparePrompt verifies (under the write lock) that the worker is still in a
// promptable state and records prompt_intent; returns the workspace + ok. If the
// worker moved, ok=false and the caller skips the prompt.
func (e *Engine) preparePrompt(ctx context.Context, workerID string, step core.StepResult) (workspace string, ok bool) {
	_ = e.Store.WithTx(ctx, func(tx core.Tx) error {
		w, err := tx.GetWorker(workerID)
		if err != nil {
			return nil
		}
		if w.State != core.WorkerRunning && w.State != core.WorkerStarting {
			return nil // moved (e.g. now waiting/blocked/terminal) → skip the prompt
		}
		workspace, ok = w.Workspace, true
		_, _, _, e2 := tx.AppendEvent(core.Event{
			Kind: "prompt_intent", WorkerID: workerID, SessionID: w.OwnerSession, Actor: "brain",
			Payload: fmt.Sprintf(`{"instruction":%q}`, step.Instruction),
		})
		return e2
	})
	return
}

func (e *Engine) transitionFromBrain(ctx context.Context, workerID string, to core.WorkerState, step core.StepResult) {
	_ = e.Store.WithTx(ctx, func(tx core.Tx) error {
		w, err := tx.GetWorker(workerID)
		if err != nil || w.State.Terminal() || !core.LegalWorkerTransition(w.State, to) {
			return nil
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
		if err != nil || w.State.Terminal() || !core.LegalWorkerTransition(w.State, core.WorkerBlocked) {
			return nil
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
