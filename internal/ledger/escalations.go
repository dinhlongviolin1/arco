package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

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
	// One pending escalation PER WORKER (a worker blocked at an `ask` cannot emit
	// a second) — dedup on worker alone so a question can't shadow a later confirm
	// or vice-versa. Safe under the single-writer serialized tx.
	if esc.WorkerID != "" {
		var existing string
		err := t.q.QueryRowContext(context.Background(),
			`SELECT id FROM escalations WHERE status='pending' AND worker_id=? ORDER BY requested_at LIMIT 1`,
			esc.WorkerID).Scan(&existing)
		if err == nil {
			return existing, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
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
	if _, err := t.q.ExecContext(context.Background(),
		`INSERT INTO escalations (id,worker_id,session_id,kind,question_class,action_class,tier,capability,
		 action_fingerprint,action,detail,draft_answer,draft_confidence,brain_rationale,answered_by,status,
		 requested_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?, 'pending', ?)`,
		esc.ID, nullStr(esc.WorkerID), nullStr(esc.SessionID), esc.Kind, esc.QuestionClass,
		string(esc.ActionClass), string(esc.Tier), nullStr(esc.Capability), esc.ActionFingerprint,
		esc.Action, esc.Detail, esc.DraftAnswer, esc.DraftConfidence, esc.BrainRationale,
		esc.AnsweredBy, t.now()); err != nil {
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

	// B14 execute-time owner re-validation: an escalation records the session that
	// was the worker's owner when it OPENED, but ownership may have transferred
	// since (release/claim/transfer). The grant + authority must be evaluated
	// against the worker's CURRENT owner, not the stale recorded session. Fetch
	// the worker once and derive the effective session from its live owner.
	var worker core.Worker
	haveWorker := false
	if esc.WorkerID != "" {
		w, werr := t.GetWorker(esc.WorkerID)
		if werr != nil && !errors.Is(werr, core.ErrNotFound) {
			return werr
		}
		if werr == nil {
			worker, haveWorker = w, true
		}
	}
	// For a worker-scoped escalation the grant tracks the LIVE worker's owner; if
	// the worker has since vanished there is no live owner, so promote nothing
	// (an empty effSession disables wouldGrant) rather than granting to the stale
	// recorded session. A session-scoped escalation (no worker) uses its session.
	effSession := esc.SessionID
	if esc.WorkerID != "" {
		effSession = ""
		if haveWorker {
			effSession = worker.OwnerSession
		}
	}

	// A grant is promoted only on a resuming decision (question, or an approved
	// confirm) with scope=session and a capability. A rejection never grants — so
	// the high-blast gate must NOT block a rejection. A worker now owned by the
	// pool (released, unowned) has no real session to hold a standing grant, so it
	// never promotes (falls back to once).
	resumes := wantKind == "question" || (wantKind == "confirm" && yes)
	wouldGrant := scope == core.ScopeSession && esc.Capability != "" &&
		effSession != "" && effSession != core.PoolSessionID && resumes
	if wouldGrant {
		row, ok, err := t.Capability(esc.Capability)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("ledger: cannot grant unknown capability %q via an escalation", esc.Capability) // fail closed
		}
		// Block a standing grant when EITHER the capability is high-blast in the
		// catalog OR the escalation itself is recorded danger/high-blast — the
		// recorded tier/class is authoritative, not decorative (opus review). This
		// keeps a worker from laundering a probed deny-listed / danger action into a
		// session grant on one operator approval; such grants require an explicit CLI grant.
		if row.HighBlast || esc.Tier == core.TierHighBlast || esc.ActionClass == core.ClassDanger {
			return core.ErrHighBlastScope
		}
	}

	status, decision := "answered", "answered"
	if wantKind == "confirm" {
		if yes {
			status, decision = "approved", "approved"
		} else {
			status, decision = "rejected", "rejected"
		}
	}

	// Promote the grant first so we know whether it actually happened (and thus
	// whether once_or_always should truthfully say "always").
	granted := false
	if wouldGrant {
		if _, err := t.Grant(effSession, esc.Capability, "human:escalation", core.Event{
			Kind: "grant", SessionID: effSession, Payload: `{"via":"escalation"}`,
		}); err != nil {
			return err
		}
		granted = true
	}
	onceAlways := "once"
	if granted {
		onceAlways = "always"
	}

	// Resume the worker: question / approved confirm → running; rejected → blocked.
	target := core.WorkerRunning
	if wantKind == "confirm" && !yes {
		target = core.WorkerBlocked
	}
	resumedAt := ""
	transitioned := false
	if haveWorker && core.LegalWorkerTransition(worker.State, target) {
		if err := t.TransitionWorker(esc.WorkerID, target, worker.Rev, e); err != nil {
			return err
		}
		transitioned = true
		if target == core.WorkerRunning {
			resumedAt = t.now()
		}
	}
	if !transitioned {
		if err := t.appendChecked(e); err != nil {
			return err
		}
	}

	_, err = t.q.ExecContext(context.Background(),
		`UPDATE escalations SET status=?, decision=?, answer_text=?, decided_by='human',
		 answered_by='human', once_or_always=?, decided_at=?, resumed_at=? WHERE id=? AND status='pending'`,
		status, decision, text, onceAlways, t.now(), nullStr(resumedAt), id)
	return err
}

// ExpirePendingForWorker closes any pending escalation for a worker that has
// left its waiting state by another path (e.g. a herdr signal), so it doesn't
// linger as a phantom. Returns the number expired.
func (t *txn) ExpirePendingForWorker(workerID string) (int, error) {
	res, err := t.q.ExecContext(context.Background(),
		`UPDATE escalations SET status='expired', decided_at=? WHERE status='pending' AND worker_id=?`,
		t.now(), workerID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
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
