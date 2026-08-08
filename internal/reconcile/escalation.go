package reconcile

import (
	"context"
	"fmt"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/notify"
)

// AnswerQuestion resolves a pending QUESTION escalation by a human and, when that
// resumes the worker, DELIVERS the answer text to its agent. The ledger resume
// alone leaves the agent parked at its input prompt with the answer never typed
// in (whole-system audit MED-2), so the worker sits idle and a later sweep can
// mis-finalize it. Delivery is async + best-effort (off the API response path),
// exactly like the initial-task delivery on the spawn path.
func (e *Engine) AnswerQuestion(ctx context.Context, id, text string, scope core.Scope) error {
	// Pre-read for the POST-COMMIT card (the worker the answer reaches). A read
	// failure here means the tx below fails too — no card on a failed answer.
	esc, escErr := e.Store.Reader().GetEscalation(id)
	if err := e.Store.WithTx(ctx, func(tx core.Tx) error {
		return tx.AnswerQuestion(id, text, scope, core.Event{Kind: "question_esc", Actor: "operator", Payload: `{"decided_by":"human"}`})
	}); err != nil {
		return err
	}
	if escErr == nil {
		e.notifyCard(esc.SessionID, notify.Card{
			Level: notify.LevelInfo,
			Title: "arco: escalation answered — " + esc.WorkerID,
			Body:  fmt.Sprintf("worker: %s\nanswer: %s", esc.WorkerID, text),
		})
	}
	e.deliverDecision(id, text)
	return nil
}

// DecideConfirm resolves a pending CONFIRM escalation by a human. An approval
// resumes the worker and is delivered to its agent (a rejection blocks the worker
// and delivers nothing — it must NOT proceed).
func (e *Engine) DecideConfirm(ctx context.Context, id string, yes bool, scope core.Scope) error {
	// Pre-read for the POST-COMMIT card (the worker the decision reaches). A read
	// failure here means the tx below fails too — no card on a failed decision.
	esc, escErr := e.Store.Reader().GetEscalation(id)
	if err := e.Store.WithTx(ctx, func(tx core.Tx) error {
		return tx.DecideConfirm(id, yes, scope, core.Event{Kind: "confirm_dec", Actor: "operator", Payload: `{"decided_by":"human"}`})
	}); err != nil {
		return err
	}
	if escErr == nil {
		decision := "rejected"
		if yes {
			decision = "approved"
		}
		e.notifyCard(esc.SessionID, notify.Card{
			Level: notify.LevelInfo,
			Title: "arco: escalation answered — " + esc.WorkerID,
			Body:  fmt.Sprintf("worker: %s\ndecision: %s", esc.WorkerID, decision),
		})
	}
	if yes {
		e.deliverDecision(id, "Approved — continue.")
	}
	return nil
}

// deliverDecision delivers a human escalation decision to the worker's agent, but
// ONLY when the decision actually resumed it: the worker must now be `running`
// with a captured pane, and never the pool sentinel (a pool-owned worker is inert
// and was not resumed — MED-4). Async via the per-worker Exec so the API response
// doesn't block on herdr; best-effort — a delivery failure is recorded as an error
// event, never surfaced as a decision failure (the ledger is already consistent
// and the worker stays re-promptable). Recovery of a delivery lost to a crash in
// this window is MED-3's job (documented follow-up).
func (e *Engine) deliverDecision(escID, text string) {
	if text == "" {
		return
	}
	esc, err := e.Store.Reader().GetEscalation(escID)
	if err != nil || esc.WorkerID == "" {
		return
	}
	// Re-read post-commit and deliver ONLY to a worker this decision resumed
	// (running, non-pool). This is a best-effort GATE, not a proof: the state could
	// drift again before the Exec closure fires (or between here and it). That's
	// acceptable — delivery is best-effort and same-pane, so a stale send can only
	// re-prompt this worker's own agent, never leak across workers; durable
	// crash-window recovery is MED-3 (documented follow-up).
	w, err := e.Store.Reader().GetWorker(esc.WorkerID)
	if err != nil || w.State != core.WorkerRunning || w.OwnerSession == core.PoolSessionID {
		return
	}
	target := promptTarget(w)
	wid := w.ID
	deliver := func() {
		e.NoteSelfPaneOp(target) // arco-caused pane activity — excluded from the D9 back-off
		if err := e.VM.PromptReady(e.bg(), target, promptIntentText(text)); err != nil {
			e.errorEvent(e.bg(), wid, "escalation answer delivery failed: "+err.Error())
		}
	}
	if e.Exec != nil {
		e.Exec.Submit(wid, deliver)
	} else {
		deliver()
	}
}
