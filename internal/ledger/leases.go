package ledger

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/dinhlongviolin1/arco/internal/admission"
	"github.com/dinhlongviolin1/arco/internal/core"
)

// secondFmt is RFC3339 truncated to whole seconds (always 'Z' at UTC). Used only
// to build a COARSE, lexically-safe superset cutoff for the start-rate prefilter
// — RFC3339Nano trims trailing fractional zeros, so a full-precision string
// compare is not chronologically ordered within a second. Exact filtering is
// done in Go against the parsed instant.
const secondFmt = "2006-01-02T15:04:05Z07:00"

// GetPool returns a provider pool by id (ErrNotFound if absent).
func (r *reader) GetPool(id string) (core.ProviderPool, error) {
	row := r.q.QueryRowContext(context.Background(),
		`SELECT id,provider,org,clavis_profile,model_class,max_active,max_starts_per_min,state,cooldown_until,created_at
		   FROM provider_pools WHERE id=?`, id)
	var p core.ProviderPool
	var org, model, state string
	var cooldown sql.NullString
	err := row.Scan(&p.ID, &p.Provider, &org, &p.ClavisProfile, &model,
		&p.MaxActive, &p.MaxStartsPerMin, &state, &cooldown, &p.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return core.ProviderPool{}, core.ErrNotFound
	}
	if err != nil {
		return core.ProviderPool{}, err
	}
	p.Org, p.ModelClass, p.State, p.CooldownUntil = org, model, core.PoolState(state), cooldown.String
	return p, nil
}

// CountActiveLeases returns the number of un-released leases held by poolID.
func (r *reader) CountActiveLeases(poolID string) (int, error) {
	var n int
	err := r.q.QueryRowContext(context.Background(),
		`SELECT COUNT(1) FROM worker_pool_leases WHERE pool_id=? AND released_at IS NULL`, poolID).Scan(&n)
	return n, err
}

// CountActiveWorkers returns the number of NON-terminal workers a session owns
// (delegation fan-in denominator). Terminal set mirrors core.WorkerState.Terminal.
func (r *reader) CountActiveWorkers(sessionID string) (int, error) {
	var n int
	err := r.q.QueryRowContext(context.Background(),
		`SELECT COUNT(1) FROM workers WHERE owner_session=?
		   AND state NOT IN ('completed_verified','failed','killed','lost')`, sessionID).Scan(&n)
	return n, err
}

// BindAgentRef records the VM-backend agent handle (herdr pane_id) captured at
// launch. ErrNotFound if the worker is gone. Does not bump rev (observation, not
// a state change).
func (t *txn) BindAgentRef(workerID, ref string) error {
	res, err := t.q.ExecContext(context.Background(),
		`UPDATE workers SET agent_ref=? WHERE id=?`, ref, workerID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return core.ErrNotFound
	}
	return nil
}

// BindLaunch records what provisioning + launch produced for a worker: its
// worktree path, the checked-out base commit, and the backend agent handle
// (herdr pane_id). Set in the dispatch_done tx. ErrNotFound if the worker is gone.
func (t *txn) BindLaunch(workerID, worktree, base, ref string) error {
	res, err := t.q.ExecContext(context.Background(),
		`UPDATE workers SET worktree=?, base_commit=?, agent_ref=? WHERE id=?`,
		worktree, base, ref, workerID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return core.ErrNotFound
	}
	return nil
}

// CountActiveWorkersOnVM returns the number of NON-terminal workers assigned to a
// VM (per-VM concurrency-admission denominator). Same terminal set as above.
func (r *reader) CountActiveWorkersOnVM(vm string) (int, error) {
	var n int
	err := r.q.QueryRowContext(context.Background(),
		`SELECT COUNT(1) FROM workers WHERE vm=?
		   AND state NOT IN ('completed_verified','failed','killed','lost')`, vm).Scan(&n)
	return n, err
}

// AcquireLease atomically admits an UNBOUND lease against poolID under the
// single-writer lock: it reads the active + start-window counts and applies the
// pure admission decision, so the count→insert is race-free (no MaxActive TOCTOU).
func (t *txn) AcquireLease(leaseID, poolID string, ttl time.Duration) error {
	pool, err := t.GetPool(poolID)
	if err != nil {
		return err // ErrNotFound if the pool doesn't exist
	}
	nowStr := t.now()
	now, err := time.Parse(time.RFC3339Nano, nowStr)
	if err != nil {
		return err
	}
	active, err := t.CountActiveLeases(poolID)
	if err != nil {
		return err
	}
	starts, err := t.countStartsInWindow(poolID, now)
	if err != nil {
		return err
	}
	dec := admission.Admit(pool, active, starts, now)
	if !dec.OK {
		return &core.LeaseRejection{Reason: dec.Reason}
	}
	// An expired cooldown that just admitted lazily clears back to ok (so state
	// reflects reality and the reader stops reporting a stale cooldown).
	if pool.State == core.PoolCooldown {
		if _, err := t.q.ExecContext(context.Background(),
			`UPDATE provider_pools SET state='ok', cooldown_until=NULL WHERE id=?`, poolID); err != nil {
			return err
		}
	}
	expires := now.Add(ttl).UTC().Format(time.RFC3339Nano)
	_, err = t.q.ExecContext(context.Background(),
		`INSERT INTO worker_pool_leases(id,pool_id,worker_id,dispatch_intent_event_id,acquired_at,expires_at,released_at)
		 VALUES(?,?,NULL,NULL,?,?,NULL)`, leaseID, poolID, nowStr, expires)
	return err
}

// countStartsInWindow counts leases acquired within the last StartRateWindow. It
// coarse-prefilters in SQL (one second of slack, whole-second cutoff → a safe
// lexical superset) and filters exactly in Go against the parsed instant, so the
// scanned set is bounded to ~one window regardless of total lease history.
func (t *txn) countStartsInWindow(poolID string, now time.Time) (int, error) {
	cutoff := now.Add(-core.StartRateWindow)
	pre := cutoff.Add(-time.Second).UTC().Format(secondFmt)
	rows, err := t.q.QueryContext(context.Background(),
		`SELECT acquired_at FROM worker_pool_leases WHERE pool_id=? AND acquired_at>=?`, poolID, pre)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var at string
		if err := rows.Scan(&at); err != nil {
			return 0, err
		}
		ts, err := time.Parse(time.RFC3339Nano, at)
		if err != nil {
			continue // unparseable row can't be proven in-window; skip (safe direction)
		}
		if !ts.Before(cutoff) {
			n++
		}
	}
	return n, rows.Err()
}

// BindLease attaches an acquired (still-active) lease to the worker + intent
// event it admitted. ErrNotFound if the lease is missing or already released.
// A non-positive event id binds NULL (event ids start at 1; 0 = "no event").
func (t *txn) BindLease(leaseID, workerID string, dispatchIntentEventID int64) error {
	var eventID any
	if dispatchIntentEventID > 0 {
		eventID = dispatchIntentEventID
	}
	res, err := t.q.ExecContext(context.Background(),
		`UPDATE worker_pool_leases SET worker_id=?, dispatch_intent_event_id=?
		   WHERE id=? AND released_at IS NULL`, workerID, eventID, leaseID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return core.ErrNotFound
	}
	return nil
}

// ReleaseLease marks a lease released. Idempotent: releasing an already-released
// (or absent) lease is a no-op, not an error.
func (t *txn) ReleaseLease(leaseID string) error {
	_, err := t.q.ExecContext(context.Background(),
		`UPDATE worker_pool_leases SET released_at=? WHERE id=? AND released_at IS NULL`, t.now(), leaseID)
	return err
}

// SetPoolState sets a pool's admission state. cooldownUntil is stored only for
// PoolCooldown; "" (or any non-cooldown state) clears it. ErrNotFound if absent.
func (t *txn) SetPoolState(poolID string, state core.PoolState, cooldownUntil string) error {
	if state != core.PoolCooldown {
		cooldownUntil = ""
	}
	res, err := t.q.ExecContext(context.Background(),
		`UPDATE provider_pools SET state=?, cooldown_until=? WHERE id=?`,
		string(state), nullStr(cooldownUntil), poolID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return core.ErrNotFound
	}
	return nil
}

// ReapLeases releases leaked/stale leases (build-guide B10-lease): an UNBOUND
// lease past its TTL (crash between acquire and CreateWorker), or a BOUND lease
// whose worker has reached a terminal state or no longer exists. A live,
// non-terminal worker's lease is NEVER reaped (its slot is still occupied).
func (t *txn) ReapLeases() (int, error) {
	now := t.now()
	// NB: the orphan clause uses NOT EXISTS, not NOT IN — NOT IN against a
	// subquery that could yield a NULL id (SQLite permits NULL in a TEXT PRIMARY
	// KEY) evaluates to NULL for every row and would silently disable orphan
	// reaping. NOT EXISTS is NULL-safe (opus review, defensive).
	res, err := t.q.ExecContext(context.Background(),
		`UPDATE worker_pool_leases SET released_at=?
		   WHERE released_at IS NULL AND (
		         (worker_id IS NULL AND expires_at < ?)
		      OR worker_id IN (SELECT id FROM workers WHERE state IN ('completed_verified','failed','killed','lost'))
		      OR (worker_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM workers w WHERE w.id = worker_pool_leases.worker_id))
		   )`, now, now)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
