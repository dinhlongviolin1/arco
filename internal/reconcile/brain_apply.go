package reconcile

import (
	"context"
	"fmt"

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
	// Persist brain INTENT before the call (audit: "decided to ask"). NOTE: full
	// crash re-drive of a dangling brain_intent is a later pass (boot recovery
	// does not yet consume it).
	_ = e.Store.WithTx(ctx, func(tx core.Tx) error {
		_, _, _, e2 := tx.AppendEvent(core.Event{
			Kind: "brain_intent", WorkerID: workerID, SessionID: w.OwnerSession, Actor: "brain",
			Payload: fmt.Sprintf(`{"state":%q}`, w.State),
		})
		return e2
	})

	res := brain.Invoke(ctx, brain.Config{Profile: e.Brain.Profile, Model: e.Brain.Model},
		assemblePrompt(w), e.Brain.Runner)

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

// assemblePrompt builds a minimal, side-effect-free decision prompt. (A richer,
// byte-stable session-transcript assembly is a later pass; this keeps the wiring
// honest without inventing context.)
func assemblePrompt(w core.Worker) string {
	return fmt.Sprintf(
		"Worker %s state=%s task=%q. Decide the next step and reply with a JSON StepResult "+
			"{\"kind\":\"run_again|dispatch|handoff|final_output|question|confirm\",\"instruction\":\"...\",\"reason\":\"...\"}.",
		w.ID, w.State, w.Task)
}

// applyStep reconciles a StepResult in a fresh tx (re-validating rev). Every
// StepResult.Kind has a branch; an unhandled kind is an error event, not a
// silent drop.
func (e *Engine) applyStep(ctx context.Context, workerID string, step core.StepResult) {
	// run_again/dispatch prompt the worker — a side effect done BEFORE the tx so
	// a tx never holds while shelling out.
	switch step.Kind {
	case "run_again", "dispatch":
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
