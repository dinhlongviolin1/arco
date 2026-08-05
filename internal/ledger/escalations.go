package ledger

import (
	"context"
	"database/sql"
	"errors"

	"github.com/oklog/ulid/v2"

	"github.com/dinhlongviolin1/arco/internal/core"
)

const escCols = `id,worker_id,session_id,kind,question_class,action_class,tier,capability,` +
	`action_fingerprint,action,detail,draft_answer,draft_confidence,brain_rationale,answered_by,` +
	`status,decision,answer_text,decided_by,once_or_always,requested_at,decided_at,resumed_at`

func scanEsc(sc scanner) (core.Escalation, error) {
	var e core.Escalation
	var worker, session, cap, decidedAt, resumedAt sql.NullString
	err := sc.Scan(
		&e.ID, &worker, &session, &e.Kind, &e.QuestionClass, &e.ActionClass, &e.Tier, &cap,
		&e.ActionFingerprint, &e.Action, &e.Detail, &e.DraftAnswer, &e.DraftConfidence, &e.BrainRationale,
		&e.AnsweredBy, &e.Status, &e.Decision, &e.AnswerText, &e.DecidedBy, &e.OnceOrAlways,
		&e.RequestedAt, &decidedAt, &resumedAt,
	)
	if err != nil {
		return core.Escalation{}, err
	}
	e.WorkerID, e.SessionID, e.Capability = worker.String, session.String, cap.String
	e.DecidedAt, e.ResumedAt = decidedAt.String, resumedAt.String
	return e, nil
}

func (r *reader) GetEscalation(id string) (core.Escalation, error) {
	row := r.q.QueryRowContext(context.Background(), `SELECT `+escCols+` FROM escalations WHERE id=?`, id)
	e, err := scanEsc(row)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Escalation{}, core.ErrNotFound
	}
	return e, err
}

func (r *reader) ListEscalations(f core.EscalationFilter) ([]core.Escalation, error) {
	q := `SELECT ` + escCols + ` FROM escalations`
	var where []string
	var args []any
	if f.Status != "" {
		where = append(where, "status=?")
		args = append(args, f.Status)
	}
	if f.SessionID != "" {
		where = append(where, "session_id=?")
		args = append(args, f.SessionID)
	}
	if f.WorkerID != "" {
		where = append(where, "worker_id=?")
		args = append(args, f.WorkerID)
	}
	if len(where) > 0 {
		q += " WHERE " + joinAnd(where)
	}
	q += " ORDER BY requested_at, id"
	rows, err := r.q.QueryContext(context.Background(), q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.Escalation
	for rows.Next() {
		e, err := scanEsc(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// OpenEscalation inserts a pending escalation. One pending per worker+capability:
// if one already exists, return its id (idempotent) rather than erroring.
func (t *txn) OpenEscalation(esc core.Escalation) (string, error) {
	// existing pending for this worker + capability?
	var existing string
	err := t.q.QueryRowContext(context.Background(),
		`SELECT id FROM escalations WHERE status='pending' AND worker_id=? AND COALESCE(capability,'')=COALESCE(?,'')`,
		nullStr(esc.WorkerID), nullStr(esc.Capability)).Scan(&existing)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if esc.ID == "" {
		esc.ID = ulid.Make().String()
	}
	if esc.Kind == "" {
		esc.Kind = "question"
	}
	if esc.QuestionClass == "" {
		esc.QuestionClass = "other"
	}
	if esc.ActionClass == "" {
		esc.ActionClass = core.ClassAmbiguous
	}
	if esc.Tier == "" {
		esc.Tier = core.TierMedium
	}
	if esc.Detail == "" {
		esc.Detail = "{}"
	}
	if esc.OnceOrAlways == "" {
		esc.OnceOrAlways = "once"
	}
	_, err = t.q.ExecContext(context.Background(),
		`INSERT INTO escalations (id,worker_id,session_id,kind,question_class,action_class,tier,capability,
		 action_fingerprint,action,detail,draft_answer,draft_confidence,brain_rationale,answered_by,status,
		 requested_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?, 'pending', ?)`,
		esc.ID, nullStr(esc.WorkerID), nullStr(esc.SessionID), esc.Kind, esc.QuestionClass,
		string(esc.ActionClass), string(esc.Tier), nullStr(esc.Capability), esc.ActionFingerprint,
		esc.Action, esc.Detail, esc.DraftAnswer, esc.DraftConfidence, esc.BrainRationale,
		esc.AnsweredBy, t.now())
	if err != nil {
		return "", err
	}
	return esc.ID, nil
}

func (t *txn) AnswerQuestion(id, text string, scope core.Scope, e core.Event) error {
	return t.decide(id, "question", true, text, scope, e)
}

func (t *txn) DecideConfirm(id string, yes bool, scope core.Scope, e core.Event) error {
	return t.decide(id, "confirm", yes, "", scope, e)
}

// decide resolves a pending escalation by a HUMAN, resumes the worker, and (for
// scope=session on a non-high-blast capability) promotes a standing grant.
func (t *txn) decide(id, wantKind string, yes bool, text string, scope core.Scope, e core.Event) error {
	esc, err := t.GetEscalation(id)
	if err != nil {
		return err
	}
	if esc.Kind != wantKind || esc.Status != "pending" {
		return core.ErrEscalationState
	}
	// scope=session promotes a standing grant — but never for a high-blast cap.
	if scope == core.ScopeSession && esc.Capability != "" {
		row, ok, err := t.Capability(esc.Capability)
		if err != nil {
			return err
		}
		if ok && row.HighBlast {
			return core.ErrHighBlastScope
		}
	}

	status := "answered"
	decision := "answered"
	if wantKind == "confirm" {
		if yes {
			status, decision = "approved", "approved"
		} else {
			status, decision = "rejected", "rejected"
		}
	}
	if _, err := t.q.ExecContext(context.Background(),
		`UPDATE escalations SET status=?, decision=?, answer_text=?, decided_by='human',
		 answered_by='human', once_or_always=?, decided_at=? WHERE id=? AND status='pending'`,
		status, decision, text, scopeToOnceAlways(scope), t.now(), id); err != nil {
		return err
	}

	// Resume the worker: question/approved confirm → running; rejected → blocked.
	if esc.WorkerID != "" {
		w, err := t.GetWorker(esc.WorkerID)
		if err == nil {
			target := core.WorkerRunning
			if wantKind == "confirm" && !yes {
				target = core.WorkerBlocked
			}
			if core.LegalWorkerTransition(w.State, target) {
				if err := t.TransitionWorker(esc.WorkerID, target, w.Rev, e); err != nil {
					return err
				}
			} else {
				if err := t.appendChecked(e); err != nil {
					return err
				}
			}
		} else if !errors.Is(err, core.ErrNotFound) {
			return err
		}
	} else if err := t.appendChecked(e); err != nil {
		return err
	}

	// Promote a standing grant when asked (non-high-blast, verified above).
	grant := (scope == core.ScopeSession) && esc.Capability != "" && esc.SessionID != ""
	if wantKind == "confirm" && !yes {
		grant = false
	}
	if grant {
		if _, err := t.Grant(esc.SessionID, esc.Capability, "human:escalation", core.Event{
			Kind: "grant", SessionID: esc.SessionID, Payload: `{"via":"escalation"}`,
		}); err != nil && !errors.Is(err, core.ErrProtectedPool) {
			return err
		}
	}
	return nil
}

func scopeToOnceAlways(s core.Scope) string {
	if s == core.ScopeSession {
		return "always"
	}
	return "once"
}

func joinAnd(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += " AND "
		}
		out += p
	}
	return out
}
