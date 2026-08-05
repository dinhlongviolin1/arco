package ledger

import (
	"context"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
)

// mkSession creates an active work session and returns its id.
func mkSession(t *testing.T, s *Store) string {
	t.Helper()
	id := ulid.Make().String()
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.CreateSession(core.Session{ID: id, Goal: "g", Status: core.SessionActive, Kind: core.SessionKindWork})
	}))
	return id
}

// mkWorker creates a running worker owned by sessionID with a non-empty
// permissions_hash (so we can assert it's cleared on reassignment).
func mkWorker(t *testing.T, s *Store, sessionID string) string {
	t.Helper()
	id := ulid.Make().String()
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		if err := tx.CreateWorker(core.Worker{
			ID: id, OwnerSession: sessionID, State: core.WorkerStarting,
			Workspace: "arco_" + id, Task: "t", RunReason: "dispatch", PermissionsHash: "sha:seed",
		}); err != nil {
			return err
		}
		w, err := tx.GetWorker(id)
		if err != nil {
			return err
		}
		return tx.TransitionWorker(id, core.WorkerRunning, w.Rev, core.Event{Kind: "dispatch_done", WorkerID: id, SessionID: sessionID, Payload: "{}"})
	}))
	return id
}

func getWorker(t *testing.T, s *Store, id string) core.Worker {
	t.Helper()
	w, err := s.Reader().GetWorker(id)
	require.NoError(t, err)
	return w
}

func TestOwnership_ReleaseToPool(t *testing.T) {
	s := newTestStore(t)
	sid := mkSession(t, s)
	wid := mkWorker(t, s, sid)

	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error { return tx.ReleaseWorker(wid, "cli") }))
	w := getWorker(t, s, wid)
	require.Equal(t, core.PoolSessionID, w.OwnerSession)
	require.NotEmpty(t, w.PooledAt)
	require.Empty(t, w.PermissionsHash, "compiled config invalidated on reassign")

	// idempotent: releasing an already-pooled worker is a no-op
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error { return tx.ReleaseWorker(wid, "cli") }))
	require.Equal(t, core.PoolSessionID, getWorker(t, s, wid).OwnerSession)
}

func TestOwnership_ReleaseTerminalRejected(t *testing.T) {
	s := newTestStore(t)
	sid := mkSession(t, s)
	wid := mkWorker(t, s, sid)
	// drive to a terminal state
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		w, _ := tx.GetWorker(wid)
		return tx.TransitionWorker(wid, core.WorkerFailed, w.Rev, core.Event{Kind: "state_change", WorkerID: wid, SessionID: sid, Payload: "{}"})
	}))
	err := s.WithTx(context.Background(), func(tx core.Tx) error { return tx.ReleaseWorker(wid, "cli") })
	require.ErrorIs(t, err, core.ErrIllegalTransition)
}

func TestOwnership_ClaimFromPool(t *testing.T) {
	s := newTestStore(t)
	sidA, sidB := mkSession(t, s), mkSession(t, s)
	wid := mkWorker(t, s, sidA)

	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error { return tx.ReleaseWorker(wid, "cli") }))
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error { return tx.ClaimWorker(wid, sidB, "cli") }))

	w := getWorker(t, s, wid)
	require.Equal(t, sidB, w.OwnerSession)
	require.Empty(t, w.PooledAt, "pooled_at cleared on claim")
}

func TestOwnership_ClaimNonPooledRejected(t *testing.T) {
	s := newTestStore(t)
	sidA, sidB := mkSession(t, s), mkSession(t, s)
	wid := mkWorker(t, s, sidA) // owned by A, never released
	err := s.WithTx(context.Background(), func(tx core.Tx) error { return tx.ClaimWorker(wid, sidB, "cli") })
	require.ErrorIs(t, err, core.ErrNotPooled)
}

func TestOwnership_ClaimIntoPoolRejected(t *testing.T) {
	s := newTestStore(t)
	sid := mkSession(t, s)
	wid := mkWorker(t, s, sid)
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error { return tx.ReleaseWorker(wid, "cli") }))
	err := s.WithTx(context.Background(), func(tx core.Tx) error { return tx.ClaimWorker(wid, core.PoolSessionID, "cli") })
	require.ErrorIs(t, err, core.ErrProtectedPool)
}

func TestOwnership_TransferBetweenSessions(t *testing.T) {
	s := newTestStore(t)
	sidA, sidB := mkSession(t, s), mkSession(t, s)
	wid := mkWorker(t, s, sidA)

	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error { return tx.TransferWorker(wid, sidB, "cli") }))
	require.Equal(t, sidB, getWorker(t, s, wid).OwnerSession)

	// self-transfer is an idempotent no-op
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error { return tx.TransferWorker(wid, sidB, "cli") }))

	// transferring a pooled worker is rejected (use claim)
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error { return tx.ReleaseWorker(wid, "cli") }))
	err := s.WithTx(context.Background(), func(tx core.Tx) error { return tx.TransferWorker(wid, sidA, "cli") })
	require.ErrorIs(t, err, core.ErrProtectedPool)
}

func TestOwnership_EventsEmitted(t *testing.T) {
	s := newTestStore(t)
	sidA, sidB := mkSession(t, s), mkSession(t, s)
	wid := mkWorker(t, s, sidA)
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error { return tx.ReleaseWorker(wid, "cli") }))
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error { return tx.ClaimWorker(wid, sidB, "cli") }))

	events, err := s.Reader().EventsSince(0, 1000)
	require.NoError(t, err)
	kinds := map[string]bool{}
	for _, e := range events {
		if e.WorkerID == wid {
			kinds[e.Kind] = true
		}
	}
	for _, k := range []string{"worker_release_intent", "worker_released", "worker_claim_intent", "worker_claimed"} {
		require.True(t, kinds[k], "expected event %s", k)
	}
}

// B14: after ownership transfers, an escalation opened under the OLD owner must
// promote its scope=session grant to the worker's CURRENT owner, not the stale
// recorded session.
func TestOwnership_B14_GrantFollowsCurrentOwner(t *testing.T) {
	s := newTestStore(t)
	sidA, sidB := mkSession(t, s), mkSession(t, s)
	wid := mkWorker(t, s, sidA)

	// escalation opened while the worker is owned by A, recording session A.
	var escID string
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		id, err := tx.OpenEscalation(core.Escalation{
			WorkerID: wid, SessionID: sidA, Kind: "question", QuestionClass: "clarify",
			ActionClass: core.ClassAmbiguous, Tier: core.TierMedium, Capability: "net.fetch", Action: "fetch",
		})
		escID = id
		return err
	}))
	// worker is transferred to B before the human answers.
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error { return tx.TransferWorker(wid, sidB, "cli") }))

	// human answers with scope=session → grant must land on B (current owner).
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.AnswerQuestion(escID, "yes go ahead", core.ScopeSession, core.Event{Kind: "question_ans", WorkerID: wid, SessionID: sidB, Payload: "{}"})
	}))

	okB, err := s.Reader().Allowed(sidB, "net.fetch")
	require.NoError(t, err)
	require.True(t, okB, "grant follows the worker to its current owner B")
	okA, err := s.Reader().Allowed(sidA, "net.fetch")
	require.NoError(t, err)
	require.False(t, okA, "the stale escalation session A must NOT receive the grant")
}

// B14: a worker RELEASED to the pool before the answer has no real owner, so a
// scope=session answer must NOT promote a standing grant (falls back to once).
func TestOwnership_B14_PooledWorkerNoGrant(t *testing.T) {
	s := newTestStore(t)
	sidA := mkSession(t, s)
	wid := mkWorker(t, s, sidA)
	var escID string
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		id, err := tx.OpenEscalation(core.Escalation{
			WorkerID: wid, SessionID: sidA, Kind: "question", Capability: "net.fetch", Action: "fetch",
		})
		escID = id
		return err
	}))
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error { return tx.ReleaseWorker(wid, "cli") }))
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.AnswerQuestion(escID, "ok", core.ScopeSession, core.Event{Kind: "question_ans", WorkerID: wid, SessionID: core.PoolSessionID, Payload: "{}"})
	}))
	okA, err := s.Reader().Allowed(sidA, "net.fetch")
	require.NoError(t, err)
	require.False(t, okA, "a released worker's answer grants nothing to the old session")
}

func TestOwnership_ReapPooledPausesAfterTTL(t *testing.T) {
	s := newTestStore(t)
	clk := &testClock{t: time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)}
	s.SetClock(clk.now)
	sid := mkSession(t, s)
	wid := mkWorker(t, s, sid)
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error { return tx.ReleaseWorker(wid, "cli") }))

	// before TTL: no pause
	var n int
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		var err error
		n, err = tx.ReapPooledWorkers(24 * time.Hour)
		return err
	}))
	require.Equal(t, 0, n)
	require.Equal(t, core.WorkerRunning, getWorker(t, s, wid).State)

	// after TTL: paused
	clk.advance(25 * time.Hour)
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		var err error
		n, err = tx.ReapPooledWorkers(24 * time.Hour)
		return err
	}))
	require.Equal(t, 1, n)
	require.Equal(t, core.WorkerPaused, getWorker(t, s, wid).State)

	// Regression (opus MED): a second reap must NOT re-pause the already-paused
	// worker (paused→paused self-edge would otherwise churn rev + spam events).
	revBefore := getWorker(t, s, wid).Rev
	countEvents := func() int {
		evs, err := s.Reader().EventsSince(0, 100000)
		require.NoError(t, err)
		n := 0
		for _, e := range evs {
			if e.WorkerID == wid && e.Payload == `{"reason":"pool_ttl_pause"}` {
				n++
			}
		}
		return n
	}
	require.Equal(t, 1, countEvents())
	clk.advance(25 * time.Hour)
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		var err error
		n, err = tx.ReapPooledWorkers(24 * time.Hour)
		return err
	}))
	require.Equal(t, 0, n, "already-paused pooled worker is not re-paused")
	require.Equal(t, revBefore, getWorker(t, s, wid).Rev, "no rev churn")
	require.Equal(t, 1, countEvents(), "no duplicate pool_ttl_pause event")
}

// NB: the worker-vanished B14 branch (grant refused when a worker-scoped
// escalation's worker row is gone) is defensive-only and not unit-tested: FK
// constraints (escalations.worker_id / events.worker_id → workers.id) keep the
// worker row alive for as long as the escalation exists, so the branch is
// unreachable in normal operation. The code guards it regardless.
