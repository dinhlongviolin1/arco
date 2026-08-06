package ledger

import (
	"context"
	"database/sql"
	"errors"
	"strings"

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
	rows, err := r.q.QueryContext(context.Background(),
		`SELECT capability,action_class,tier,default_allowed,high_blast,compiled_worker,description
		 FROM capability_catalog WHERE default_allowed<>0 ORDER BY capability`)
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
