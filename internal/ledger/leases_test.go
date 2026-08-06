package ledger

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/admission"
	"github.com/dinhlongviolin1/arco/internal/core"
)

// testClock is a mutable, race-safe injectable clock.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *testClock) advance(d time.Duration) { c.mu.Lock(); defer c.mu.Unlock(); c.t = c.t.Add(d) }

func seedPool(t *testing.T, s *Store, id string, maxActive, maxStarts int) {
	t.Helper()
	_, err := s.DB().Exec(
		`INSERT INTO provider_pools(id,provider,org,clavis_profile,model_class,max_active,max_starts_per_min,state,cooldown_until,created_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?)`,
		id, "anthropic", "", "default", "", maxActive, maxStarts, "ok", nil, s.now())
	require.NoError(t, err)
}

func acquire(t *testing.T, s *Store, leaseID, poolID string, ttl time.Duration) error {
	t.Helper()
	return s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.AcquireLease(leaseID, poolID, ttl)
	})
}

const ttl = 15 * time.Minute

func TestLease_AcquireBindReleaseRoundTrip(t *testing.T) {
	s := newTestStore(t)
	seedPool(t, s, "p1", 35, 10)

	lid := ulid.Make().String()
	require.NoError(t, acquire(t, s, lid, "p1", ttl))

	n, err := s.Reader().CountActiveLeases("p1")
	require.NoError(t, err)
	require.Equal(t, 1, n)

	// bind to a real worker + intent event
	sid, wid := ulid.Make().String(), ulid.Make().String()
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		if err := tx.CreateSession(core.Session{ID: sid, Goal: "g", Status: core.SessionActive, Kind: core.SessionKindWork}); err != nil {
			return err
		}
		if err := tx.CreateWorker(core.Worker{ID: wid, OwnerSession: sid, State: core.WorkerStarting, Workspace: "arco_" + wid, Task: "t", RunReason: "dispatch"}); err != nil {
			return err
		}
		cur, _, _, err := tx.AppendEvent(core.Event{Kind: "dispatch_intent", SessionID: sid, WorkerID: wid, Actor: "cli", Payload: "{}"})
		if err != nil {
			return err
		}
		return tx.BindLease(lid, wid, cur)
	}))

	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error { return tx.ReleaseLease(lid) }))
	n, err = s.Reader().CountActiveLeases("p1")
	require.NoError(t, err)
	require.Equal(t, 0, n)

	// release is idempotent
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error { return tx.ReleaseLease(lid) }))
}

func TestLease_CapacityRejectsAtMax(t *testing.T) {
	s := newTestStore(t)
	seedPool(t, s, "p1", 2, 100) // cap = 2 active

	l1, l2, l3 := ulid.Make().String(), ulid.Make().String(), ulid.Make().String()
	require.NoError(t, acquire(t, s, l1, "p1", ttl))
	require.NoError(t, acquire(t, s, l2, "p1", ttl))

	err := acquire(t, s, l3, "p1", ttl)
	require.ErrorIs(t, err, core.ErrLeaseRejected)
	var rej *core.LeaseRejection
	require.ErrorAs(t, err, &rej)
	require.Equal(t, admission.ReasonAtCapacity, rej.Reason)

	// freeing a slot re-admits
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error { return tx.ReleaseLease(l1) }))
	require.NoError(t, acquire(t, s, l3, "p1", ttl))
}

func TestLease_StartRateRejectsThenRecovers(t *testing.T) {
	s := newTestStore(t)
	clk := &testClock{t: time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)}
	s.SetClock(clk.now)
	seedPool(t, s, "p1", 100, 2) // huge capacity, but only 2 starts/min

	require.NoError(t, acquire(t, s, ulid.Make().String(), "p1", ttl))
	require.NoError(t, acquire(t, s, ulid.Make().String(), "p1", ttl))

	err := acquire(t, s, ulid.Make().String(), "p1", ttl)
	require.ErrorIs(t, err, core.ErrLeaseRejected)
	var rej *core.LeaseRejection
	require.ErrorAs(t, err, &rej)
	require.Equal(t, admission.ReasonStartRate, rej.Reason)

	// slide past the window → the two prior starts age out, admission resumes
	clk.advance(core.StartRateWindow + time.Second)
	require.NoError(t, acquire(t, s, ulid.Make().String(), "p1", ttl))
}

// The single-writer lock must make count→insert atomic: with cap=5 and 20
// concurrent acquirers, EXACTLY 5 succeed and MaxActive is never exceeded.
func TestLease_ConcurrentAcquireNeverExceedsMax(t *testing.T) {
	s := newTestStore(t)
	seedPool(t, s, "p1", 5, 100)

	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, rejected := 0, 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := acquire(t, s, ulid.Make().String(), "p1", ttl)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			default:
				require.ErrorIs(t, err, core.ErrLeaseRejected)
				rejected++
			}
		}()
	}
	wg.Wait()
	require.Equal(t, 5, ok)
	require.Equal(t, 15, rejected)
	n, err := s.Reader().CountActiveLeases("p1")
	require.NoError(t, err)
	require.Equal(t, 5, n)
}

func TestLease_CooldownBlocksThenAutoClears(t *testing.T) {
	s := newTestStore(t)
	clk := &testClock{t: time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)}
	s.SetClock(clk.now)
	seedPool(t, s, "p1", 35, 10)

	until := clk.now().Add(30 * time.Second).UTC().Format(time.RFC3339Nano)
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.SetPoolState("p1", core.PoolCooldown, until)
	}))

	err := acquire(t, s, ulid.Make().String(), "p1", ttl)
	require.ErrorIs(t, err, core.ErrLeaseRejected)
	var rej *core.LeaseRejection
	require.ErrorAs(t, err, &rej)
	require.Equal(t, admission.ReasonCooldown, rej.Reason)

	// past the deadline: acquisition succeeds AND the pool state lazily clears.
	clk.advance(31 * time.Second)
	require.NoError(t, acquire(t, s, ulid.Make().String(), "p1", ttl))
	p, err := s.Reader().GetPool("p1")
	require.NoError(t, err)
	require.Equal(t, core.PoolOK, p.State)
	require.Empty(t, p.CooldownUntil)
}

func TestLease_DisabledPoolRejects(t *testing.T) {
	s := newTestStore(t)
	seedPool(t, s, "p1", 35, 10)
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.SetPoolState("p1", core.PoolDisabled, "")
	}))
	err := acquire(t, s, ulid.Make().String(), "p1", ttl)
	require.ErrorIs(t, err, core.ErrLeaseRejected)
	var rej *core.LeaseRejection
	require.ErrorAs(t, err, &rej)
	require.Equal(t, admission.ReasonDisabled, rej.Reason)
}

func TestLease_AcquireUnknownPool(t *testing.T) {
	s := newTestStore(t)
	require.ErrorIs(t, acquire(t, s, ulid.Make().String(), "nope", ttl), core.ErrNotFound)
}

func TestLease_BindMissingLease(t *testing.T) {
	s := newTestStore(t)
	err := s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.BindLease("ghost", "w", 1)
	})
	require.ErrorIs(t, err, core.ErrNotFound)
}

// Reaping: an unbound lease past TTL is reaped; a lease bound to a terminal
// worker is reaped; a lease bound to a LIVE (non-terminal) worker is NEVER
// reaped even if its TTL has passed (the slot is still occupied).
func TestLease_ReapLeakedAndTerminalButNotLive(t *testing.T) {
	s := newTestStore(t)
	clk := &testClock{t: time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)}
	s.SetClock(clk.now)
	seedPool(t, s, "p1", 100, 100)

	// (a) unbound lease with a short TTL — will leak.
	leaked := ulid.Make().String()
	require.NoError(t, acquire(t, s, leaked, "p1", time.Minute))

	// (b) lease bound to a worker we drive terminal.
	termLease := ulid.Make().String()
	sid, termWorker := ulid.Make().String(), ulid.Make().String()
	require.NoError(t, acquire(t, s, termLease, "p1", 24*time.Hour))
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		if err := tx.CreateSession(core.Session{ID: sid, Goal: "g", Status: core.SessionActive, Kind: core.SessionKindWork}); err != nil {
			return err
		}
		if err := tx.CreateWorker(core.Worker{ID: termWorker, OwnerSession: sid, State: core.WorkerStarting, Workspace: "arco_" + termWorker, Task: "t", RunReason: "dispatch"}); err != nil {
			return err
		}
		return tx.BindLease(termLease, termWorker, 0)
	}))

	// (c) lease bound to a LIVE worker, short TTL — must survive reaping.
	liveLease := ulid.Make().String()
	liveWorker := ulid.Make().String()
	require.NoError(t, acquire(t, s, liveLease, "p1", time.Minute))
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		if err := tx.CreateWorker(core.Worker{ID: liveWorker, OwnerSession: sid, State: core.WorkerStarting, Workspace: "arco_" + liveWorker, Task: "t", RunReason: "dispatch"}); err != nil {
			return err
		}
		return tx.BindLease(liveLease, liveWorker, 0)
	}))

	// drive the terminal worker terminal.
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		w, err := tx.GetWorker(termWorker)
		if err != nil {
			return err
		}
		return tx.TransitionWorker(termWorker, core.WorkerFailed, w.Rev, core.Event{Kind: "dispatch_done", WorkerID: termWorker, SessionID: sid, Payload: "{}"})
	}))

	// advance past the short TTLs so the unbound + live leases are both expired.
	clk.advance(2 * time.Minute)

	var reaped int
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		var err error
		reaped, err = tx.ReapLeases()
		return err
	}))
	require.Equal(t, 2, reaped, "leaked-unbound + terminal-bound reaped; live-bound spared")

	// The live worker's lease is still active (its slot is still held).
	n, err := s.Reader().CountActiveLeases("p1")
	require.NoError(t, err)
	require.Equal(t, 1, n)
}

func TestCreatePool_AndListPools(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	require.NoError(t, s.WithTx(ctx, func(tx core.Tx) error {
		return tx.CreatePool(core.ProviderPool{ID: "p1", ClavisProfile: "deepseek-1", Provider: "deepseek"})
	}))
	// defaults applied
	p, err := s.Reader().GetPool("p1")
	require.NoError(t, err)
	require.Equal(t, "deepseek-1", p.ClavisProfile)
	require.Equal(t, core.PoolOK, p.State)
	require.Equal(t, 35, p.MaxActive, "schema default max_active")

	// a second pool + list (id-ordered)
	require.NoError(t, s.WithTx(ctx, func(tx core.Tx) error {
		return tx.CreatePool(core.ProviderPool{ID: "p0", ClavisProfile: "qwen-1", MaxActive: 5})
	}))
	pools, err := s.Reader().ListPools()
	require.NoError(t, err)
	require.Len(t, pools, 2)
	require.Equal(t, "p0", pools[0].ID)
	require.Equal(t, 5, pools[0].MaxActive)
	require.Equal(t, "p1", pools[1].ID)

	// missing required field
	err = s.WithTx(ctx, func(tx core.Tx) error { return tx.CreatePool(core.ProviderPool{ID: "bad"}) })
	require.Error(t, err, "clavis_profile required")
}
