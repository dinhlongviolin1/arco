package ledger

import (
	"context"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
)

// StaleBrainIntents returns running/starting, non-pool workers whose MOST-RECENT
// brain_intent has no event sharing its correlation_id and is older than the
// cutoff — a classification lost to a crash before it acted. This matrix pins the
// correctness the crash-recovery re-drive depends on.
func TestStaleBrainIntents(t *testing.T) {
	s := newTestStore(t)
	clk := &testClock{t: time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)}
	s.SetClock(clk.now)
	ctx := context.Background()

	sid := ulid.Make().String()
	// each worker exercises one branch of the predicate
	dangling := ulid.Make().String()    // brain_intent(cid), no sibling → STALE
	resolved := ulid.Make().String()    // brain_intent(cid) + prompt_intent(cid) → resolved
	blocked := ulid.Make().String()     // dangling but state=blocked → excluded
	pool := ulid.Make().String()        // dangling but owner=pool → excluded
	superseded := ulid.Make().String()  // old resolved cid THEN a newer dangling cid → STALE
	interleaved := ulid.Make().String() // dangling + a later unrelated (no-cid) event → STALE
	fresh := ulid.Make().String()       // brain_intent(cid) recorded after the cutoff → excluded

	mk := func(tx core.Tx, id, owner string, st core.WorkerState) error {
		return tx.CreateWorker(core.Worker{ID: id, OwnerSession: owner, State: st, Workspace: "arco_" + id, Task: "t", RunReason: "x"})
	}
	bi := func(tx core.Tx, wid, owner, cid string) error {
		_, _, _, err := tx.AppendEvent(core.Event{Kind: "brain_intent", WorkerID: wid, SessionID: owner, Actor: "brain", CorrelationID: cid})
		return err
	}

	require.NoError(t, s.WithTx(ctx, func(tx core.Tx) error {
		if err := tx.CreateSession(core.Session{ID: sid, Goal: "g", Status: core.SessionActive, Kind: core.SessionKindWork}); err != nil {
			return err
		}
		if err := mk(tx, dangling, sid, core.WorkerRunning); err != nil {
			return err
		}
		if err := bi(tx, dangling, sid, "cid-dangling"); err != nil {
			return err
		}
		// resolved: brain_intent then a prompt_intent carrying the SAME cid
		if err := mk(tx, resolved, sid, core.WorkerRunning); err != nil {
			return err
		}
		if err := bi(tx, resolved, sid, "cid-resolved"); err != nil {
			return err
		}
		if _, _, _, err := tx.AppendEvent(core.Event{Kind: "prompt_intent", WorkerID: resolved, SessionID: sid, Actor: "brain", CorrelationID: "cid-resolved"}); err != nil {
			return err
		}
		if err := mk(tx, blocked, sid, core.WorkerBlocked); err != nil {
			return err
		}
		if err := bi(tx, blocked, sid, "cid-blocked"); err != nil {
			return err
		}
		if err := mk(tx, pool, core.PoolSessionID, core.WorkerRunning); err != nil {
			return err
		}
		if err := bi(tx, pool, core.PoolSessionID, "cid-pool"); err != nil {
			return err
		}
		// superseded: an OLD resolved classification, then a NEWER dangling one
		if err := mk(tx, superseded, sid, core.WorkerRunning); err != nil {
			return err
		}
		if err := bi(tx, superseded, sid, "cid-old"); err != nil {
			return err
		}
		if _, _, _, err := tx.AppendEvent(core.Event{Kind: "brain_dispatch", WorkerID: superseded, SessionID: sid, Actor: "brain", CorrelationID: "cid-old"}); err != nil {
			return err
		}
		if err := bi(tx, superseded, sid, "cid-new"); err != nil {
			return err
		}
		// interleaved: dangling brain_intent then an unrelated event with NO cid
		if err := mk(tx, interleaved, sid, core.WorkerRunning); err != nil {
			return err
		}
		if err := bi(tx, interleaved, sid, "cid-inter"); err != nil {
			return err
		}
		_, _, _, err := tx.AppendEvent(core.Event{Kind: "audit_denied", WorkerID: interleaved, SessionID: sid, Actor: "worker", Payload: "{}"})
		return err
	}))

	// fresh: recorded 10 minutes later (after the cutoff)
	clk.advance(10 * time.Minute)
	require.NoError(t, s.WithTx(ctx, func(tx core.Tx) error {
		if err := mk(tx, fresh, sid, core.WorkerRunning); err != nil {
			return err
		}
		return bi(tx, fresh, sid, "cid-fresh")
	}))

	ids, err := s.Reader().StaleBrainIntents(time.Date(2026, 8, 6, 12, 5, 0, 0, time.UTC))
	require.NoError(t, err)
	require.ElementsMatch(t, []string{dangling, superseded, interleaved}, ids)
	require.NotContains(t, ids, resolved, "a cid-sibling (fired side effect) resolves the intent")
	require.NotContains(t, ids, blocked, "non-running workers are excluded")
	require.NotContains(t, ids, pool, "pool-owned workers are excluded")
	require.NotContains(t, ids, fresh, "an intent fresher than the cutoff is excluded")
}

// A dangling intent whose worker was re-driven and RATE-LIMITED within the grace
// window backs off (excluded) so an over-cap session's stale worker isn't
// re-submitted every sweep; once the throttle ages past the grace it re-drives.
func TestStaleBrainIntents_RateLimitBackoff(t *testing.T) {
	s := newTestStore(t)
	clk := &testClock{t: time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)}
	s.SetClock(clk.now)
	ctx := context.Background()

	sid := ulid.Make().String()
	wid := ulid.Make().String()
	require.NoError(t, s.WithTx(ctx, func(tx core.Tx) error {
		if err := tx.CreateSession(core.Session{ID: sid, Goal: "g", Status: core.SessionActive, Kind: core.SessionKindWork}); err != nil {
			return err
		}
		if err := tx.CreateWorker(core.Worker{ID: wid, OwnerSession: sid, State: core.WorkerRunning, Workspace: "arco_" + wid, Task: "t", RunReason: "x"}); err != nil {
			return err
		}
		_, _, _, err := tx.AppendEvent(core.Event{Kind: "brain_intent", WorkerID: wid, SessionID: sid, Actor: "brain", CorrelationID: "cid"})
		return err
	}))
	// A throttled re-drive attempt at T0+4m (no cid — doesn't resolve the intent).
	clk.advance(4 * time.Minute)
	require.NoError(t, s.WithTx(ctx, func(tx core.Tx) error {
		_, _, _, err := tx.AppendEvent(core.Event{Kind: "brain_rate_limited", WorkerID: wid, SessionID: sid, Actor: "brain", Payload: "{}"})
		return err
	}))

	// Cutoff = T0+3m: the intent (T0) is old enough, but the throttle (T0+4m) is
	// NEWER than the cutoff → within the grace → back off, don't re-drive yet.
	ids, err := s.Reader().StaleBrainIntents(time.Date(2026, 8, 6, 12, 3, 0, 0, time.UTC))
	require.NoError(t, err)
	require.NotContains(t, ids, wid, "recently throttled → backs off")

	// Cutoff = T0+6m: the throttle (T0+4m) is now OLDER than the cutoff → aged past
	// the grace → re-drive.
	ids, err = s.Reader().StaleBrainIntents(time.Date(2026, 8, 6, 12, 6, 0, 0, time.UTC))
	require.NoError(t, err)
	require.Contains(t, ids, wid, "throttle aged past the grace → re-driven")
}
