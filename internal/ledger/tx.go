package ledger

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/oklog/ulid/v2"

	"github.com/dinhlongviolin1/arco/internal/core"
)

// txn implements core.Tx. It embeds a reader bound to the *sql.Tx so reads see
// uncommitted writes, and adds the write methods (CAS-guarded).
type txn struct {
	*reader
	q   *sql.Tx
	now func() string
}

var _ core.Tx = (*txn)(nil)

func newTxn(q *sql.Tx, now func() string) *txn {
	return &txn{reader: &reader{q: q}, q: q, now: now}
}

func (t *txn) CreateWorker(w core.Worker) error {
	ts := t.now()
	if w.CreatedAt == "" {
		w.CreatedAt = ts
	}
	if w.LastSeenAt == "" {
		w.LastSeenAt = ts
	}
	if w.LastEventAt == "" {
		w.LastEventAt = ts
	}
	if w.State == "" {
		w.State = core.WorkerStarting
	}
	if w.OwnerSession == "" {
		return fmt.Errorf("ledger: CreateWorker requires owner_session")
	}
	var pid any
	if w.PID != nil {
		pid = *w.PID
	}
	_, err := t.q.ExecContext(context.Background(),
		`INSERT INTO workers (`+workerCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		w.ID, w.Title, w.VM, w.Workspace, w.Worktree, w.BaseCommit, w.HeadCommit, w.Program, w.AgentKind,
		w.BootID, pid, w.PIDStartTime, string(w.State), w.Rev, w.StallCount, w.OwnerSession, w.SessionPermRev,
		w.PermissionsHash, w.CompiledConfig, w.Task, w.RunReason, nullStr(w.ParentWorkerID), w.DelegationDepth,
		w.Role, w.Summary, w.LastSeenAt, w.LastEventAt, nullStr(w.PooledAt), w.CreatedAt,
	)
	return err
}

// TransitionWorker guards the transition, applies it under an optimistic CAS on
// rev, and appends the event in the same tx. No side effect without a CAS win.
func (t *txn) TransitionWorker(id string, to core.WorkerState, expectedRev int64, e core.Event) error {
	cur, err := t.GetWorker(id)
	if err != nil {
		return err
	}
	if !core.LegalWorkerTransition(cur.State, to) {
		return fmt.Errorf("%w: %s->%s", core.ErrIllegalTransition, cur.State, to)
	}
	res, err := t.q.ExecContext(context.Background(),
		`UPDATE workers SET state=?, rev=rev+1, last_event_at=? WHERE id=? AND rev=?`,
		string(to), t.now(), id, expectedRev)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return core.ErrRevMismatch
	}
	return t.appendChecked(e)
}

// AppendEvent appends idempotently. Identity = (source, source_event_id).
// Re-delivery of the same id is a no-op (deduped=true). A same-id/different-hash
// arrival records an `error` event and returns conflict=true (never a silent dedup).
func (t *txn) AppendEvent(e core.Event) (cursor int64, deduped bool, conflict bool, err error) {
	if e.Source == "" {
		e.Source = "internal"
	}
	if e.OccurredAt == "" {
		e.OccurredAt = t.now()
	}
	e.RecordedAt = t.now()

	// Internal events (no source id) never dedup — insert straight.
	if e.SourceEventID != "" {
		var existingHash sql.NullString
		var exists bool
		row := t.q.QueryRowContext(context.Background(),
			`SELECT source_event_hash FROM events WHERE source=? AND source_event_id=?`, e.Source, e.SourceEventID)
		switch err := row.Scan(&existingHash); err {
		case nil:
			exists = true
		case sql.ErrNoRows:
			exists = false
		default:
			return 0, false, false, err
		}
		if exists {
			if existingHash.String == e.SourceEventHash {
				return 0, true, false, nil // idempotent no-op
			}
			// same id, different hash → record an error event, signal conflict.
			// The error event carries a STABLE synthetic id so repeated poisoned
			// re-deliveries dedup to a single error row (no amplification).
			if _, _, _, ierr := t.insertEvent(core.Event{
				Source: "internal", SourceEventID: e.Source + ":" + e.SourceEventID + ":conflict",
				Kind: "error", SessionID: e.SessionID, WorkerID: e.WorkerID,
				Payload: fmt.Sprintf(`{"error":"source_event_hash mismatch","source":%q,"source_event_id":%q}`,
					e.Source, e.SourceEventID),
			}); ierr != nil {
				return 0, false, false, ierr
			}
			return 0, false, true, nil
		}
	}
	return t.insertEvent(e)
}

func (t *txn) insertEvent(e core.Event) (int64, bool, bool, error) {
	if e.OccurredAt == "" {
		e.OccurredAt = t.now()
	}
	if e.RecordedAt == "" {
		e.RecordedAt = t.now()
	}
	if e.Source == "" {
		e.Source = "internal"
	}
	// ON CONFLICT DO NOTHING backstops the check-then-insert dedup with the real
	// UNIQUE(source, source_event_id) constraint (NULL source_event_id never
	// conflicts, so internal events always insert).
	res, err := t.q.ExecContext(context.Background(),
		`INSERT INTO events (`+eventCols+`) VALUES (NULL,?,?,?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(source, source_event_id) DO NOTHING`,
		e.Source, nullStr(e.SourceEventID), nullStr(e.SourceEventHash), nullStr(e.WorkerID),
		nullStr(e.SessionID), e.Kind, e.Actor, e.CausationEventID, nullStr(e.CorrelationID),
		e.Payload, e.OccurredAt, e.RecordedAt)
	if err != nil {
		return 0, false, false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, true, false, nil // dedup backstop hit
	}
	id, _ := res.LastInsertId()
	return id, false, false, nil
}

// ObserveWorker records liveness/HEAD without a state change or rev bump.
func (t *txn) ObserveWorker(id string, obs core.WorkerObservation) error {
	sets := []string{}
	args := []any{}
	if obs.HeadCommit != "" {
		sets = append(sets, "head_commit=?")
		args = append(args, obs.HeadCommit)
	}
	if obs.PIDStartTime != "" {
		sets = append(sets, "pid_start_time=?")
		args = append(args, obs.PIDStartTime)
	}
	if obs.BootID != "" {
		sets = append(sets, "boot_id=?")
		args = append(args, obs.BootID)
	}
	if obs.PID != nil {
		sets = append(sets, "pid=?")
		args = append(args, *obs.PID)
	}
	last := obs.LastSeenAt
	if last == "" {
		last = t.now()
	}
	sets = append(sets, "last_seen_at=?")
	args = append(args, last)
	args = append(args, id)
	res, err := t.q.ExecContext(context.Background(),
		`UPDATE workers SET `+strings.Join(sets, ", ")+` WHERE id=?`, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return core.ErrNotFound
	}
	return nil
}

// appendChecked appends an internal event and treats a hash-conflict as an
// error so the surrounding CAS tx rolls back rather than committing a state
// change with no corresponding event (build-guide "never a silent dedup").
func (t *txn) appendChecked(e core.Event) error {
	_, _, conflict, err := t.AppendEvent(e)
	if err != nil {
		return err
	}
	if conflict {
		return fmt.Errorf("ledger: event conflict (hash mismatch) for %s:%s", e.Source, e.SourceEventID)
	}
	return nil
}

func (t *txn) CreateSession(s core.Session) error {
	ts := t.now()
	if s.CreatedAt == "" {
		s.CreatedAt = ts
	}
	if s.LastActivityAt == "" {
		s.LastActivityAt = ts
	}
	if s.Status == "" {
		s.Status = core.SessionOpen
	}
	if s.Kind == "" {
		s.Kind = core.SessionKindWork
	}
	if s.Permissions == "" {
		s.Permissions = "{}"
	}
	if s.NotifyLevel == "" {
		s.NotifyLevel = "important"
	}
	pinned := 0
	if s.Pinned {
		pinned = 1
	}
	_, err := t.q.ExecContext(context.Background(),
		`INSERT INTO sessions (id,slug,title,goal,status,kind,parent_session,rev,perm_rev,mem_rev,permissions,
		 context_summary,context_rev,facts,progress,repo,default_vm,pinned,notify_level,tg_topic_id,
		 tg_status_msg_id,stall_count,last_activity_at,created_at,closed_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		s.ID, nullStr(s.Slug), s.Title, s.Goal, string(s.Status), string(s.Kind), nullStr(s.ParentSession),
		s.Rev, s.PermRev, s.MemRev, s.Permissions, s.ContextSummary, s.ContextRev, s.Facts, s.Progress,
		s.Repo, s.DefaultVM, pinned, s.NotifyLevel, s.TGTopicID, s.TGStatusMsgID, s.StallCount,
		s.LastActivityAt, s.CreatedAt, nullStr(s.ClosedAt))
	return err
}

func (t *txn) SetSessionStatus(id string, to core.SessionStatus, expectedRev int64, e core.Event) error {
	cur, err := t.GetSession(id)
	if err != nil {
		return err
	}
	if cur.Kind == core.SessionKindPool {
		return core.ErrProtectedPool
	}
	if !core.LegalSessionTransition(cur.Status, to) {
		return fmt.Errorf("%w: session %s->%s", core.ErrIllegalTransition, cur.Status, to)
	}
	res, err := t.q.ExecContext(context.Background(),
		`UPDATE sessions SET status=?, rev=rev+1, last_activity_at=? WHERE id=? AND rev=?`,
		string(to), t.now(), id, expectedRev)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return core.ErrRevMismatch
	}
	return t.appendChecked(e)
}

func (t *txn) AttachWorker(sessionID, workerID string) error {
	// Validate the target: it must exist and not be the protected pool (release-
	// to-pool is a PASS-3 op with its own path, not a bare attach).
	s, err := t.GetSession(sessionID)
	if err != nil {
		return err
	}
	if s.Kind == core.SessionKindPool {
		return core.ErrProtectedPool
	}
	res, err := t.q.ExecContext(context.Background(),
		`UPDATE workers SET owner_session=? WHERE id=?`, sessionID, workerID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return core.ErrNotFound
	}
	return t.appendChecked(core.Event{
		Kind: "note", SessionID: sessionID, WorkerID: workerID, Payload: `{"attach":true}`,
	})
}

// Grant adds an active standing grant, bumps the session perm_rev, and appends
// the event — all in one tx. Rejected on the protected pool session.
func (t *txn) Grant(sessionID, capability, grantedBy string, e core.Event) (int64, error) {
	s, err := t.GetSession(sessionID)
	if err != nil {
		return 0, err
	}
	if s.Kind == core.SessionKindPool {
		return 0, core.ErrProtectedPool
	}
	if _, ok, err := t.Capability(capability); err != nil {
		return 0, err
	} else if !ok {
		return 0, fmt.Errorf("ledger: unknown capability %q", capability)
	}
	// Idempotent: an existing active grant is a no-op (no duplicate row, no
	// perm_rev churn that would force needless worker recompiles).
	var existing int
	if err := t.q.QueryRowContext(context.Background(),
		`SELECT COUNT(1) FROM session_grants WHERE session_id=? AND capability=? AND status='active'`,
		sessionID, capability).Scan(&existing); err != nil {
		return 0, err
	}
	if existing > 0 {
		return s.PermRev, nil
	}
	newRev := s.PermRev + 1
	_, err = t.q.ExecContext(context.Background(),
		`INSERT INTO session_grants (id,session_id,capability,status,scope,granted_by,created_perm_rev,created_at)
		 VALUES (?,?,?,'active','session',?,?,?)`,
		ulid.Make().String(), sessionID, capability, grantedBy, newRev, t.now())
	if err != nil {
		return 0, err
	}
	if err := t.bumpPermRev(sessionID, newRev); err != nil {
		return 0, err
	}
	if err := t.appendChecked(e); err != nil {
		return 0, err
	}
	return newRev, nil
}

// Revoke marks the capability's active grants revoked across the session AND its
// descendants (child ⊆ parent theorem; build-guide §A #2 cascade-materialize),
// bumping each affected session's perm_rev.
func (t *txn) Revoke(sessionID, capability string, e core.Event) (int64, error) {
	ids, err := t.subtreeIDs(sessionID)
	if err != nil {
		return 0, err
	}
	// Track the root's perm_rev; only bump a session (and emit the event) when a
	// grant was actually revoked there — a no-op revoke must not churn perm_rev.
	root, err := t.GetSession(sessionID)
	if err != nil {
		return 0, err
	}
	rootRev := root.PermRev
	revokedAny := false
	for _, id := range ids {
		res, err := t.q.ExecContext(context.Background(),
			`UPDATE session_grants SET status='revoked', revoked_at=?
			 WHERE session_id=? AND capability=? AND status='active'`, t.now(), id, capability)
		if err != nil {
			return 0, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			revokedAny = true
			s, err := t.GetSession(id)
			if err != nil {
				return 0, err
			}
			if err := t.bumpPermRev(id, s.PermRev+1); err != nil {
				return 0, err
			}
			if id == sessionID {
				rootRev = s.PermRev + 1
			}
		}
	}
	if revokedAny {
		if err := t.appendChecked(e); err != nil {
			return 0, err
		}
	}
	return rootRev, nil
}

func (t *txn) bumpPermRev(sessionID string, newRev int64) error {
	_, err := t.q.ExecContext(context.Background(),
		`UPDATE sessions SET perm_rev=? WHERE id=?`, newRev, sessionID)
	return err
}

// subtreeIDs returns sessionID and all descendants via a recursive CTE (also
// the cycle-safe basis for move-subtree later).
func (t *txn) subtreeIDs(root string) ([]string, error) {
	rows, err := t.q.QueryContext(context.Background(),
		`WITH RECURSIVE sub(id) AS (
		    SELECT id FROM sessions WHERE id=?
		    UNION
		    SELECT s.id FROM sessions s JOIN sub ON s.parent_session = sub.id
		 ) SELECT id FROM sub`, root)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
