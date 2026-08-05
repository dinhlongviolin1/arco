package ledger

import (
	"context"
	"fmt"
	"time"

	"github.com/dinhlongviolin1/arco/internal/core"
)

// This file implements worker ownership transfer (build-guide-rev6 PASS-3):
// release → pool, claim from pool, direct session→session transfer, plus the
// pool-TTL reaper. Ownership is a single-owner invariant (workers.owner_session);
// the pool sentinel (core.PoolSessionID) holds released, unowned workers.
//
// On each release/claim/transfer op below (via reassign) the compiled config is
// invalidated (permissions_hash cleared) so the reconcile recompiles the
// worker's capability tree against its NEW owner before it acts again, and rev
// is bumped so any in-flight CAS that assumed the old owner fails. arco's
// authoritative Allowed() gate always reads the CURRENT owner_session (B14), so
// a stale worker-side config is layer-1 only. (AttachWorker is a distinct,
// currently-unused primitive that does NOT carry these invariants.)

// ReleaseWorker hands a live worker back to the pool.
func (t *txn) ReleaseWorker(workerID, actor string) error {
	w, err := t.GetWorker(workerID)
	if err != nil {
		return err
	}
	if w.State.Terminal() {
		return fmt.Errorf("%w: cannot release a %s worker", core.ErrIllegalTransition, w.State)
	}
	if w.OwnerSession == core.PoolSessionID {
		return nil // already pooled — idempotent no-op (no duplicate events)
	}
	from := w.OwnerSession
	if err := t.appendChecked(core.Event{
		Kind: "worker_release_intent", WorkerID: workerID, SessionID: from, Actor: actor,
		Payload: `{"to":"pool"}`,
	}); err != nil {
		return err
	}
	if err := t.reassign(workerID, core.PoolSessionID, w.Rev, true); err != nil {
		return err
	}
	return t.appendChecked(core.Event{
		Kind: "worker_released", WorkerID: workerID, SessionID: core.PoolSessionID, Actor: actor,
		Payload: fmt.Sprintf(`{"from":%q}`, from),
	})
}

// ClaimWorker moves a pooled worker to toSession.
func (t *txn) ClaimWorker(workerID, toSession, actor string) error {
	if err := t.requireWorkSession(toSession); err != nil {
		return err
	}
	w, err := t.GetWorker(workerID)
	if err != nil {
		return err
	}
	if w.State.Terminal() {
		return fmt.Errorf("%w: cannot claim a %s worker", core.ErrIllegalTransition, w.State)
	}
	if w.OwnerSession != core.PoolSessionID {
		return core.ErrNotPooled
	}
	if err := t.appendChecked(core.Event{
		Kind: "worker_claim_intent", WorkerID: workerID, SessionID: toSession, Actor: actor,
		Payload: `{"from":"pool"}`,
	}); err != nil {
		return err
	}
	if err := t.reassign(workerID, toSession, w.Rev, false); err != nil {
		return err
	}
	return t.appendChecked(core.Event{
		Kind: "worker_claimed", WorkerID: workerID, SessionID: toSession, Actor: actor, Payload: "{}",
	})
}

// TransferWorker moves a worker directly between non-pool sessions.
func (t *txn) TransferWorker(workerID, toSession, actor string) error {
	if err := t.requireWorkSession(toSession); err != nil {
		return err
	}
	w, err := t.GetWorker(workerID)
	if err != nil {
		return err
	}
	if w.State.Terminal() {
		return fmt.Errorf("%w: cannot transfer a %s worker", core.ErrIllegalTransition, w.State)
	}
	if w.OwnerSession == core.PoolSessionID {
		return fmt.Errorf("%w: use claim to take a pooled worker", core.ErrProtectedPool)
	}
	if w.OwnerSession == toSession {
		return nil // already there — idempotent no-op
	}
	from := w.OwnerSession
	if err := t.appendChecked(core.Event{
		Kind: "worker_transfer_intent", WorkerID: workerID, SessionID: toSession, Actor: actor,
		Payload: fmt.Sprintf(`{"from":%q}`, from),
	}); err != nil {
		return err
	}
	if err := t.reassign(workerID, toSession, w.Rev, false); err != nil {
		return err
	}
	return t.appendChecked(core.Event{
		Kind: "worker_transferred", WorkerID: workerID, SessionID: toSession, Actor: actor,
		Payload: fmt.Sprintf(`{"from":%q}`, from),
	})
}

// reassign rewrites a worker's owner under a rev CAS, clears the compiled-config
// hash (forces recompile against the new owner), and sets/clears pooled_at.
func (t *txn) reassign(workerID, toSession string, expectedRev int64, pooling bool) error {
	var pooledAt any
	if pooling {
		pooledAt = t.now()
	}
	res, err := t.q.ExecContext(context.Background(),
		`UPDATE workers SET owner_session=?, pooled_at=?, permissions_hash='', rev=rev+1
		   WHERE id=? AND rev=?`, toSession, pooledAt, workerID, expectedRev)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return core.ErrRevMismatch // a concurrent writer moved it first
	}
	return nil
}

// requireWorkSession validates that a target session exists and is NOT the pool.
func (t *txn) requireWorkSession(sessionID string) error {
	s, err := t.GetSession(sessionID)
	if err != nil {
		return err
	}
	if s.Kind == core.SessionKindPool {
		return core.ErrProtectedPool
	}
	return nil
}

// ReapPooledWorkers pauses any worker pooled longer than ttl (an unclaimed
// worker must not run indefinitely). Only workers with a legal path to paused
// are paused; the rest are left for the sweep. pooled_at is compared as a parsed
// instant (RFC3339Nano string ranges aren't chronologically ordered).
func (t *txn) ReapPooledWorkers(ttl time.Duration) (int, error) {
	now, err := time.Parse(time.RFC3339Nano, t.now())
	if err != nil {
		return 0, err
	}
	cutoff := now.Add(-ttl)
	// Exclude already-paused workers: pausing does not clear pooled_at, and
	// paused→paused is a legal self-edge, so without this the reaper would re-pause
	// the same worker every sweep — churning rev + spamming the immutable event log
	// (opus review). A paused pooled worker stays paused until claimed.
	rows, err := t.q.QueryContext(context.Background(),
		`SELECT id, pooled_at, state, rev FROM workers WHERE pooled_at IS NOT NULL AND state<>'paused'`)
	if err != nil {
		return 0, err
	}
	type cand struct {
		id, state string
		rev       int64
	}
	var cands []cand
	for rows.Next() {
		var id, pooledAt, state string
		var rev int64
		if err := rows.Scan(&id, &pooledAt, &state, &rev); err != nil {
			rows.Close()
			return 0, err
		}
		ts, perr := time.Parse(time.RFC3339Nano, pooledAt)
		if perr != nil || !ts.Before(cutoff) {
			continue // not parseable, or not yet past TTL
		}
		if !core.LegalWorkerTransition(core.WorkerState(state), core.WorkerPaused) {
			continue
		}
		cands = append(cands, cand{id: id, state: state, rev: rev})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()

	n := 0
	for _, c := range cands {
		if err := t.TransitionWorker(c.id, core.WorkerPaused, c.rev, core.Event{
			Kind: "state_change", WorkerID: c.id, SessionID: core.PoolSessionID,
			Payload: `{"reason":"pool_ttl_pause"}`,
		}); err != nil {
			if err == core.ErrRevMismatch {
				continue // moved concurrently; next sweep re-derives
			}
			return n, err
		}
		n++
	}
	return n, nil
}
