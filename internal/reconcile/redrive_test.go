package reconcile

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
)

// settableClock is a test clock whose value can be advanced between sweeps.
type settableClock struct{ ns atomic.Int64 }

func (c *settableClock) now() time.Time  { return time.Unix(0, c.ns.Load()).UTC() }
func (c *settableClock) set(t time.Time) { c.ns.Store(t.UnixNano()) }

// A brain classification lost to a crash (brain_intent recorded, result never
// applied) is re-driven by the sweep once older than the grace.
func TestSweep_RedrivesLostBrainIntent(t *testing.T) {
	base := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	clk := &settableClock{}
	clk.set(base)
	var calls atomic.Int32

	e, s, _ := newEngine(t)
	s.SetClock(clk.now)
	e.Brain = BrainCfg{Enabled: true, Profile: "p", Model: "m",
		Runner: func(context.Context, string, ...string) ([]byte, error) {
			calls.Add(1)
			return []byte(`{"kind":"final_output","reason":"done"}`), nil
		}}

	// A running worker whose most-recent brain_intent (with a cid) has no cid-sibling.
	id := dispatchRunning(t, e)
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		w, _ := tx.GetWorker(id)
		_, _, _, err := tx.AppendEvent(core.Event{Kind: "brain_intent", WorkerID: id, SessionID: w.OwnerSession, Actor: "brain", CorrelationID: "lost-cid"})
		return err
	}))

	// Fresh → not re-driven (could still be in flight).
	res, err := e.Sweep(context.Background())
	require.NoError(t, err)
	e.Exec.Wait()
	require.Equal(t, 0, res.BrainRedrives)
	require.Equal(t, int32(0), calls.Load())

	// Aged past the grace → re-driven exactly once; the classification resolves.
	clk.set(base.Add(defaultBrainIntentGrace + time.Minute))
	res, err = e.Sweep(context.Background())
	require.NoError(t, err)
	e.Exec.Wait()
	require.Equal(t, 1, res.BrainRedrives)
	require.Equal(t, int32(1), calls.Load())
	w, _ := s.Reader().GetWorker(id)
	require.Equal(t, core.WorkerCompletedCandidate, w.State)

	// Resolved (worker terminal) → no further re-drive.
	res, err = e.Sweep(context.Background())
	require.NoError(t, err)
	require.Equal(t, 0, res.BrainRedrives)
}

// THE regression for the opus HIGH: a classification that DISPATCHED a child
// (child created + brain_dispatch(cid) written atomically) must NOT be re-driven,
// or the sweep would spawn a duplicate child for the same subtask.
func TestSweep_DoesNotRedriveCompletedDispatch(t *testing.T) {
	base := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	clk := &settableClock{}
	clk.set(base)
	var calls atomic.Int32

	e, s, _ := newEngine(t)
	s.SetClock(clk.now)
	e.Brain = BrainCfg{Enabled: true, Profile: "p", Model: "m",
		Runner: func(context.Context, string, ...string) ([]byte, error) {
			calls.Add(1)
			return []byte(`{"kind":"dispatch","instruction":"do subtask"}`), nil
		}}

	// A real classification that decides to dispatch → Delegate creates a child +
	// a parent-scoped brain_dispatch(cid) atomically.
	parent := dispatchRunning(t, e)
	require.NoError(t, e.ApplyEvent(context.Background(), ambiguousEvent(parent)))
	e.Exec.Wait()
	require.Equal(t, int32(1), calls.Load(), "brain classified once")

	children := func() int {
		ws, _ := s.Reader().ListWorkers(core.WorkerFilter{})
		n := 0
		for _, w := range ws {
			if w.ParentWorkerID == parent {
				n++
			}
		}
		return n
	}
	require.Equal(t, 1, children(), "exactly one child from the dispatch")

	// Age well past the grace and sweep: the parent's brain_intent HAS a cid-sibling
	// (brain_dispatch), so it is not dangling — no re-drive, no second child.
	clk.set(base.Add(defaultBrainIntentGrace + time.Hour))
	res, err := e.Sweep(context.Background())
	require.NoError(t, err)
	e.Exec.Wait()
	require.Equal(t, 0, res.BrainRedrives, "a completed dispatch is not dangling")
	require.Equal(t, int32(1), calls.Load(), "brain not re-invoked")
	require.Equal(t, 1, children(), "no duplicate child spawned")
}

// A classification that was PROCESSED but neither moved the worker nor fired a
// side effect (here: a fan-in-denied dispatch → error event, parent stays
// running) must NOT be re-driven forever — the brain_resolved(cid) marker
// resolves it. Without that marker the sweep would re-invoke the brain every
// grace interval on a permanently-denied classification (opus Finding 1).
func TestSweep_DoesNotRedriveProcessedNonSideEffect(t *testing.T) {
	base := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	clk := &settableClock{}
	clk.set(base)
	var calls atomic.Int32

	e, s, _ := newEngine(t)
	s.SetClock(clk.now)
	e.MaxChildren = 1 // the parent itself is the 1 active worker → any dispatch is denied
	e.Brain = BrainCfg{Enabled: true, Profile: "p", Model: "m",
		Runner: func(context.Context, string, ...string) ([]byte, error) {
			calls.Add(1)
			return []byte(`{"kind":"dispatch","instruction":"sub"}`), nil
		}}

	parent := dispatchRunning(t, e)
	require.NoError(t, e.ApplyEvent(context.Background(), ambiguousEvent(parent)))
	e.Exec.Wait()
	require.Equal(t, int32(1), calls.Load(), "classified once")
	w, _ := s.Reader().GetWorker(parent)
	require.Equal(t, core.WorkerRunning, w.State, "denied dispatch leaves the parent running")

	// Aged well past the grace: the classification was processed (brain_resolved),
	// so it is NOT dangling — no re-drive, no repeated LLM call.
	clk.set(base.Add(defaultBrainIntentGrace + time.Hour))
	res, err := e.Sweep(context.Background())
	require.NoError(t, err)
	e.Exec.Wait()
	require.Equal(t, 0, res.BrainRedrives, "a processed (denied) classification is not re-driven")
	require.Equal(t, int32(1), calls.Load(), "brain not re-invoked on a permanently-denied dispatch")
}

// Dedup: a worker with an in-flight re-drive is not re-submitted by a later
// sweep (the brain_intent marker that clears it from the dangling set is
// committed async, so without this a backed-up Exec would let a second sweep
// stack a duplicate classification → duplicate prompt/child). qwen review.
func TestSweep_RedriveDedupSkipsInFlight(t *testing.T) {
	base := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	clk := &settableClock{}
	clk.set(base)

	e, s, _ := newEngine(t)
	s.SetClock(clk.now)
	e.Brain = BrainCfg{Enabled: true, Profile: "p", Model: "m",
		Runner: func(context.Context, string, ...string) ([]byte, error) {
			return []byte(`{"kind":"final_output"}`), nil
		}}

	id := dispatchRunning(t, e)
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		w, _ := tx.GetWorker(id)
		_, _, _, err := tx.AppendEvent(core.Event{Kind: "brain_intent", WorkerID: id, SessionID: w.OwnerSession, Actor: "brain", CorrelationID: "cid"})
		return err
	}))
	clk.set(base.Add(defaultBrainIntentGrace + time.Minute)) // now the intent is stale
	// Simulate a re-drive already in flight for this worker.
	require.True(t, e.claimRedrive(id))

	n := e.redriveStaleBrainIntents()
	require.Equal(t, 0, n, "a worker with an in-flight re-drive is not re-submitted")

	// Once the in-flight one completes (marker cleared), a later sweep may re-drive.
	e.releaseRedrive(id)
	require.Equal(t, 1, e.redriveStaleBrainIntents())
	e.Exec.Wait()
}

// A BrainIntentGrace configured below 2× the call timeout is clamped up to the
// default floor, so an in-flight call can never be re-driven.
func TestSweep_GraceFlooredBelowCallTimeout(t *testing.T) {
	base := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	clk := &settableClock{}
	clk.set(base)
	var calls atomic.Int32

	e, s, _ := newEngine(t)
	s.SetClock(clk.now)
	e.BrainIntentGrace = 30 * time.Second // below 2×120s → must be clamped to the default
	e.Brain = BrainCfg{Enabled: true, Profile: "p", Model: "m",
		Runner: func(context.Context, string, ...string) ([]byte, error) {
			calls.Add(1)
			return []byte(`{"kind":"final_output","reason":"done"}`), nil
		}}

	id := dispatchRunning(t, e)
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		w, _ := tx.GetWorker(id)
		_, _, _, err := tx.AppendEvent(core.Event{Kind: "brain_intent", WorkerID: id, SessionID: w.OwnerSession, Actor: "brain", CorrelationID: "cid"})
		return err
	}))

	// 60s later: past the configured 30s but WELL under the enforced 5-min floor.
	clk.set(base.Add(60 * time.Second))
	res, err := e.Sweep(context.Background())
	require.NoError(t, err)
	e.Exec.Wait()
	require.Equal(t, 0, res.BrainRedrives, "sub-floor grace is clamped up; not re-driven at 60s")
	require.Equal(t, int32(0), calls.Load())

	// Past the floor → re-driven.
	clk.set(base.Add(defaultBrainIntentGrace + time.Minute))
	res, err = e.Sweep(context.Background())
	require.NoError(t, err)
	e.Exec.Wait()
	require.Equal(t, 1, res.BrainRedrives)
}
