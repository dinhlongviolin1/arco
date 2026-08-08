package reconcile

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/notify"
)

// AuditDeniedAttempt handles a worker's attempt at a DENY-LISTED action, reported
// via the audit tail (its PreToolUse hook denied the call; build-guide PASS-3).
// Response: record the attempt, AUTO-PAUSE the worker (stop it probing), and open
// a DANGER-class confirm escalation for a human — all in one tx.
//
// Idempotent: keyed on sourceEventID (herdr may redeliver), and the pause is
// guarded (no self-transition churn), and OpenEscalation is one-pending-per-worker.
// The escalation is high_blast, so a human can approve it ONCE but the decide path
// refuses to promote a standing grant (ErrHighBlastScope) — a probed deny-listed
// capability never becomes a session grant by this path.
func (e *Engine) AuditDeniedAttempt(ctx context.Context, workerID, capability, detail, sourceEventID string) error {
	// Bound worker-supplied fields (semi-trusted input): cap length and build the
	// payload with json.Marshal so invalid UTF-8 can't produce malformed JSON.
	capability = truncate(capability, eventPayloadCap)
	detail = truncate(detail, eventPayloadCap)
	payload, _ := json.Marshal(map[string]string{"capability": capability, "detail": detail})

	var opened bool       // the danger confirm was opened in the tx (not a redelivery)
	var task, sess string // the worker's task + session, for the decision card
	err := e.Store.WithTx(ctx, func(tx core.Tx) error {
		w, err := tx.GetWorker(workerID)
		if err != nil {
			return err
		}
		_, deduped, _, err := tx.AppendEvent(core.Event{
			Kind: "audit_denied", WorkerID: workerID, SessionID: w.OwnerSession, Actor: "worker",
			SourceEventID: sourceEventID, Payload: string(payload),
		})
		if err != nil {
			return err
		}
		if deduped {
			return nil // a redelivery of an already-handled attempt — no double pause/escalation
		}

		// Auto-pause (only if it's a real, legal, non-self transition — avoids rev
		// churn when the worker is already paused/terminal). INTENTIONAL (audit F3):
		// a worker in an unpausable state (starting mid-launch, completed_candidate,
		// lost) is NOT paused — pausing starting would race the dispatch CAS, and
		// killing would be heavy-handed for an attempt the PreToolUse hook ALREADY
		// denied. The danger escalation below still surfaces it, and a
		// subsequently-running worker is pausable on its next attempt.
		if !w.State.Terminal() && w.State != core.WorkerPaused && core.LegalWorkerTransition(w.State, core.WorkerPaused) {
			if err := tx.TransitionWorker(workerID, core.WorkerPaused, w.Rev, core.Event{
				Kind: "state_change", WorkerID: workerID, SessionID: w.OwnerSession,
				Payload: `{"reason":"deny_listed_attempt"}`,
			}); err != nil && !errors.Is(err, core.ErrRevMismatch) {
				return err
			}
		}

		// A danger attempt must SURFACE — OpenEscalation is one-pending-per-worker,
		// so a pre-existing benign question would otherwise shadow it (and answering
		// that question would resume the worker without the danger ever shown).
		// Expire any pending escalation first so the danger confirm is what the
		// operator sees (opus review). The worker is paused regardless. Expiring
		// first also means the OpenEscalation below ALWAYS creates a new row — the
		// notify card fires (no dedup is possible at this point).
		if _, err := tx.ExpirePendingForWorker(workerID); err != nil {
			return err
		}
		if _, err = tx.OpenEscalation(core.Escalation{
			WorkerID: workerID, SessionID: w.OwnerSession, Kind: "confirm",
			QuestionClass: "proceed-confirmation", ActionClass: core.ClassDanger, Tier: core.TierHighBlast,
			Capability: capability, Action: "worker attempted a deny-listed capability", Detail: detail,
		}); err != nil {
			return err
		}
		opened, task, sess = true, w.Task, w.OwnerSession
		return nil
	})
	if err != nil {
		return err
	}
	// POST-COMMIT: push the danger-confirm decision card.
	if opened {
		e.notifyCard(sess, notify.FormatEscalation(notify.EscalationCard{
			WorkerID: workerID,
			TaskTail: taskTail(task),
			Question: "worker attempted a deny-listed capability",
		}))
	}
	return nil
}
