package reconcile

import (
	"context"
	"fmt"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/notify"
)

// questionClasses is the frozen question_class enum (0001 CHECK). The earn-out
// report iterates it so an operator sees every class, tallied or not.
var questionClasses = []string{"clarify", "proceed-confirmation", "scope-change", "resource", "other"}

// earnOutConfigured reports whether the CLASS-INDEPENDENT promotion gates hold:
// a verification leg is live and both thresholds are actually set. Non-positive
// thresholds disable promotion — a zero/unset knob must never mean "promote
// instantly".
func (e *Engine) earnOutConfigured() bool {
	return e.VerificationLive && e.EarnOutMinDecisions >= 1 && e.EarnOutMinAgreement > 0
}

// classEarnedOut applies the per-class history gates to a tally.
func (e *Engine) classEarnedOut(agree, total int) bool {
	return total >= e.EarnOutMinDecisions &&
		float64(agree)/float64(total) >= e.EarnOutMinAgreement
}

// autoAnswerEarnedOut is the sweep's earn-out promotion step (rev7/T3.5): each
// pending DRAFTED question whose class has proven itself is resolved with the
// brain's own draft, attributed answered_by='brain', and the draft is delivered
// to the resumed agent exactly like a human answer (the deliverDecision path).
// Every gate must pass, re-checked inside the tx: session mode auto — the
// CURRENT owner's mode, so a T3.6 demotion pauses promotion with no extra code
// — ∧ VerificationLive ∧ non-empty draft ∧ class history at/above both
// thresholds. Confirms never enter (kind filter + AnswerQuestionBrain's kind
// check); a pool-owned or terminal worker is left for the human (MED-4); the
// auto-answer creates no grant and feeds no tally by construction. Returns how
// many questions were answered.
func (e *Engine) autoAnswerEarnedOut(ctx context.Context) int {
	if !e.earnOutConfigured() {
		return 0
	}
	pend, err := e.Store.Reader().ListEscalations(core.EscalationFilter{Status: "pending"})
	if err != nil {
		return 0 // best-effort, like the other sweep reapers
	}
	n := 0
	for _, esc := range pend {
		if esc.Kind != "question" || esc.DraftAnswer == "" || esc.WorkerID == "" {
			continue
		}
		var answered bool
		var agree, total int
		txErr := e.Store.WithTx(ctx, func(tx core.Tx) error {
			answered = false
			cur, err := tx.GetEscalation(esc.ID)
			if err != nil || cur.Status != "pending" || cur.Kind != "question" || cur.DraftAnswer == "" {
				return err // moved since the list — leave it
			}
			w, err := tx.GetWorker(cur.WorkerID)
			if err != nil {
				return nil // gone or unreadable — stays a human decision (fail closed)
			}
			if w.State.Terminal() || w.OwnerSession == core.PoolSessionID {
				return nil // never resurrect, never drive the pool sentinel (MED-4)
			}
			// D9 gate on the worker's CURRENT owner session, at sweep time.
			s, err := tx.GetSession(w.OwnerSession)
			if err != nil {
				return nil
			}
			mode, err := core.ParseSupervisionMode(string(s.SupervisionMode))
			if err != nil || !mode.Allows(core.ActBrainAct) {
				return nil // assist/manual (or unknown): the human keeps the decision
			}
			if agree, total, err = tx.DraftAgreement(cur.QuestionClass); err != nil {
				return err
			}
			if !e.classEarnedOut(agree, total) {
				return nil
			}
			// The audit event: an operator must be able to reconstruct WHY arco
			// answered by itself, from the ledger alone.
			if err := tx.AnswerQuestionBrain(cur.ID, cur.DraftAnswer, core.Event{
				Kind: "auto_answer", Actor: "brain",
				Payload: fmt.Sprintf(
					`{"decided_by":"brain","question_class":%q,"agree":%d,"total":%d,"min_decisions":%d,"min_agreement":%g}`,
					cur.QuestionClass, agree, total, e.EarnOutMinDecisions, e.EarnOutMinAgreement),
			}); err != nil {
				return err
			}
			answered = true
			return nil
		})
		if txErr != nil || !answered {
			continue
		}
		n++
		// POST-COMMIT: the operator hears about every self-answer (mode auto
		// allows ActNotify), and the draft reaches the resumed agent's pane via
		// the same path a human answer takes.
		e.notifyCard(esc.SessionID, notify.Card{
			Level: notify.LevelInfo,
			Title: "arco: escalation auto-answered — " + esc.WorkerID,
			Body: fmt.Sprintf("worker: %s\nclass: %s (%d/%d agreed)\nanswer: %s",
				esc.WorkerID, esc.QuestionClass, agree, total, esc.DraftAnswer),
		})
		e.deliverDecision(esc.ID, esc.DraftAnswer)
	}
	return n
}

// EarnOutClassReport is one question_class row of the autonomy report.
type EarnOutClassReport struct {
	Class    string
	Agree    int
	Total    int
	Promotes bool
}

// EarnOutReport reports, per frozen question_class, the human track record on
// drafted escalations and whether the class currently promotes under the live
// gates (VerificationLive ∧ thresholds ∧ history). Per-escalation gates —
// session mode auto, a non-empty draft, kind question — still apply at sweep
// time; Promotes means "an eligible drafted question of this class would be
// auto-answered".
func (e *Engine) EarnOutReport() ([]EarnOutClassReport, error) {
	r := e.Store.Reader()
	out := make([]EarnOutClassReport, 0, len(questionClasses))
	for _, c := range questionClasses {
		agree, total, err := r.DraftAgreement(c)
		if err != nil {
			return nil, err
		}
		out = append(out, EarnOutClassReport{
			Class: c, Agree: agree, Total: total,
			Promotes: e.earnOutConfigured() && e.classEarnedOut(agree, total),
		})
	}
	return out, nil
}
