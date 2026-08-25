package ledger

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/dinhlongviolin1/arco/internal/core"
)

// reader implements core.Reader over any querier (*sql.DB for standalone reads,
// or a *sql.Tx for reads inside a write transaction).
type reader struct{ q querier }

var _ core.Reader = (*reader)(nil)

func (r *reader) GetWorker(id string) (core.Worker, error) {
	row := r.q.QueryRowContext(context.Background(), `SELECT `+workerCols+` FROM workers WHERE id=?`, id)
	w, err := scanWorker(row)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Worker{}, core.ErrNotFound
	}
	return w, err
}

func (r *reader) ListWorkers(f core.WorkerFilter) ([]core.Worker, error) {
	var where []string
	var args []any
	if f.State != "" {
		where = append(where, "state=?")
		args = append(args, string(f.State))
	}
	if f.VM != "" {
		where = append(where, "vm=?")
		args = append(args, f.VM)
	}
	if f.OwnerSession != "" {
		where = append(where, "owner_session=?")
		args = append(args, f.OwnerSession)
	}
	q := `SELECT ` + workerCols + ` FROM workers`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY id"
	rows, err := r.q.QueryContext(context.Background(), q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.Worker
	for rows.Next() {
		w, err := scanWorker(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (r *reader) GetSession(id string) (core.Session, error) {
	row := r.q.QueryRowContext(context.Background(), `SELECT `+sessionCols+` FROM sessions WHERE id=?`, id)
	s, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Session{}, core.ErrNotFound
	}
	return s, err
}

// ResolveSession resolves by id first, then slug.
func (r *reader) ResolveSession(ref string) (core.Session, error) {
	if s, err := r.GetSession(ref); err == nil {
		return s, nil
	}
	row := r.q.QueryRowContext(context.Background(), `SELECT `+sessionCols+` FROM sessions WHERE slug=?`, ref)
	s, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Session{}, core.ErrNotFound
	}
	return s, err
}

func (r *reader) ListSessions(f core.SessionFilter) ([]core.Session, error) {
	var where []string
	var args []any
	if f.Status != "" {
		where = append(where, "status=?")
		args = append(args, string(f.Status))
	}
	if f.Kind != "" {
		where = append(where, "kind=?")
		args = append(args, string(f.Kind))
	}
	q := `SELECT ` + sessionCols + ` FROM sessions`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY id"
	rows, err := r.q.QueryContext(context.Background(), q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.Session
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (r *reader) RecentWorkerEvents(workerID string, limit int) ([]core.Event, error) {
	if limit <= 0 {
		limit = 20
	}
	// Newest-first with a LIMIT keeps the scan bounded, then reverse to
	// chronological order for a stable, readable transcript tail.
	rows, err := r.q.QueryContext(context.Background(),
		`SELECT `+eventCols+` FROM events WHERE worker_id=? ORDER BY id DESC LIMIT ?`, workerID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// StaleBrainIntents returns the IDs of RUNNING/STARTING, non-pool workers whose
// most-recent `brain_intent` was recorded before `before` and has NO other event
// sharing its correlation_id — a brain classification that was durably recorded
// but lost to a crash BEFORE it acted (the daemon died in the off-write-path call
// window). The sweep re-drives these so a lost classification is retried.
//
// Correctness rests on the correlation, not recency: brainClassify stamps the
// brain_intent with a unique cid, and the ONLY two decisions that leave the
// worker running with an un-recallable side effect — run_again (prompt_intent)
// and dispatch (brain_dispatch, written atomically with child creation) — stamp
// that SAME cid on their side-effect intent. So a fired side effect always
// leaves a cid-sibling and the worker drops out here, meaning a re-drive can
// never duplicate a prompt or a child. A rate-limited retry carries no cid, so
// it never falsely resolves the intent; unrelated interleaved events don't share
// the cid either. Every other decision moves the worker out of running/starting,
// so the state filter excludes it (and re-driving it would be idempotent anyway).
//
// The time compare is in Go: the candidate set is tiny (workers with an
// unresolved brain_intent), and RFC3339Nano isn't lexically chronological.
func (r *reader) StaleBrainIntents(before time.Time) ([]string, error) {
	rows, err := r.q.QueryContext(context.Background(),
		`SELECT bi.worker_id, bi.recorded_at,
		        (SELECT rl.recorded_at FROM events rl
		         WHERE rl.worker_id=bi.worker_id AND rl.kind='brain_rate_limited'
		         ORDER BY rl.id DESC LIMIT 1)
		 FROM events bi
		 JOIN workers w ON w.id = bi.worker_id
		 WHERE bi.kind='brain_intent' AND bi.correlation_id IS NOT NULL
		   AND w.state IN (?, ?) AND w.owner_session <> ?
		   AND bi.id=(SELECT MAX(id) FROM events e2 WHERE e2.worker_id=bi.worker_id AND e2.kind='brain_intent')
		   AND NOT EXISTS (SELECT 1 FROM events e3 WHERE e3.correlation_id=bi.correlation_id AND e3.id<>bi.id)`,
		string(core.WorkerRunning), string(core.WorkerStarting), core.PoolSessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id, at string
		var rlAt sql.NullString
		if err := rows.Scan(&id, &at, &rlAt); err != nil {
			return nil, err
		}
		ts, err := time.Parse(time.RFC3339Nano, at)
		if err != nil {
			continue // unparseable timestamp: skip rather than mis-drive
		}
		if !ts.Before(before) {
			continue // intent not yet older than the grace (may be in flight)
		}
		// Rate-limit back-off: if a re-drive was already throttled within the last
		// grace window, wait — else an over-cap session's stale worker would be
		// re-submitted (and re-throttled) every single sweep, not once per grace.
		if rlAt.Valid {
			if rlt, perr := time.Parse(time.RFC3339Nano, rlAt.String); perr == nil && !rlt.Before(before) {
				continue
			}
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (r *reader) EventsSince(cursor int64, limit int) ([]core.Event, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := r.q.QueryContext(context.Background(),
		`SELECT `+eventCols+` FROM events WHERE id>? ORDER BY id LIMIT ?`, cursor, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (r *reader) Capability(name string) (core.CatalogRow, bool, error) {
	row := r.q.QueryRowContext(context.Background(),
		`SELECT capability,action_class,tier,default_allowed,high_blast,compiled_worker,description
		 FROM capability_catalog WHERE capability=?`, name)
	c, err := scanCatalog(row)
	if errors.Is(err, sql.ErrNoRows) {
		return core.CatalogRow{}, false, nil
	}
	if err != nil {
		return core.CatalogRow{}, false, err
	}
	return c, true, nil
}

func (r *reader) DefaultTree() ([]core.CatalogRow, error) {
	return r.catalogWhere("WHERE default_allowed<>0")
}

// Catalog returns the FULL capability_catalog (all rows, incl. high-blast) — the
// input permcompile.Compile requires (NOT DefaultTree, which omits the high-blast
// rows the deny layer needs to see).
func (r *reader) Catalog() ([]core.CatalogRow, error) {
	return r.catalogWhere("")
}

func (r *reader) catalogWhere(where string) ([]core.CatalogRow, error) {
	rows, err := r.q.QueryContext(context.Background(),
		`SELECT capability,action_class,tier,default_allowed,high_blast,compiled_worker,description
		 FROM capability_catalog `+where+` ORDER BY capability`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.CatalogRow
	for rows.Next() {
		c, err := scanCatalog(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Allowed is the authoritative capability check for arco-executed actions.
// A capability is allowed for a session if it's in DefaultTree() OR has an
// active, unexpired grant. high_blast capabilities are never in DefaultTree,
// so they require an explicit grant. Unknown capabilities fail closed.
func (r *reader) Allowed(sessionID, capability string) (bool, error) {
	cat, ok, err := r.Capability(capability)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil // fail closed on unclassifiable
	}
	if cat.DefaultAllowed {
		return true, nil
	}
	var n int
	err = r.q.QueryRowContext(context.Background(),
		`SELECT COUNT(1) FROM session_grants
		 WHERE session_id=? AND capability=? AND status='active'
		   AND (expires_at IS NULL OR expires_at > ?)`,
		sessionID, capability, nowRFC()).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// GrantedCapabilities returns a session's EFFECTIVE granted capability set — the
// input permcompile.Compile needs. It mirrors Allowed() as a set: every
// default-allowed catalog capability, plus every capability with an active,
// non-expired session_grant for this session. Decision-free (no cascade/policy;
// just the same rule Allowed() applies, enumerated).
func (r *reader) GrantedCapabilities(sessionID string) (map[string]bool, error) {
	granted := map[string]bool{}
	scan := func(rows *sql.Rows, err error) error {
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var cap string
			if err := rows.Scan(&cap); err != nil {
				return err
			}
			granted[cap] = true
		}
		return rows.Err()
	}
	// default-allowed caps (granted to every session, matching Allowed()).
	if err := scan(r.q.QueryContext(context.Background(),
		`SELECT capability FROM capability_catalog WHERE default_allowed<>0`)); err != nil {
		return nil, err
	}
	// explicit active, non-expired grants for this session.
	if err := scan(r.q.QueryContext(context.Background(),
		`SELECT capability FROM session_grants
		 WHERE session_id=? AND status='active' AND (expires_at IS NULL OR expires_at > ?)`,
		sessionID, nowRFC())); err != nil {
		return nil, err
	}
	return granted, nil
}

// GrantedCapabilitiesForWorker is GrantedCapabilities scoped to one worker
// (issue model): default-allowed ∪ the session-wide baseline (worker_id NULL) ∪
// this worker's OWN grants — and crucially NOT sibling workers' per-worker grants,
// so an approval for one aspect doesn't leak into another. Used by the spawn path
// to compile a worker's permission surface.
func (r *reader) GrantedCapabilitiesForWorker(sessionID, workerID string) (map[string]bool, error) {
	granted := map[string]bool{}
	scan := func(rows *sql.Rows, err error) error {
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var cap string
			if err := rows.Scan(&cap); err != nil {
				return err
			}
			granted[cap] = true
		}
		return rows.Err()
	}
	if err := scan(r.q.QueryContext(context.Background(),
		`SELECT capability FROM capability_catalog WHERE default_allowed<>0`)); err != nil {
		return nil, err
	}
	if err := scan(r.q.QueryContext(context.Background(),
		`SELECT capability FROM session_grants
		 WHERE session_id=? AND status='active' AND (expires_at IS NULL OR expires_at > ?)
		   AND (worker_id IS NULL OR worker_id=?)`,
		sessionID, nowRFC(), workerID)); err != nil {
		return nil, err
	}
	return granted, nil
}

func scanCatalog(sc scanner) (core.CatalogRow, error) {
	var c core.CatalogRow
	var def, hb, cw int
	err := sc.Scan(&c.Capability, &c.ActionClass, &c.Tier, &def, &hb, &cw, &c.Description)
	if err != nil {
		return core.CatalogRow{}, err
	}
	c.DefaultAllowed, c.HighBlast, c.CompiledWorker = def != 0, hb != 0, cw != 0
	return c, nil
}
