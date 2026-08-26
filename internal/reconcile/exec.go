package reconcile

import (
	"log"
	"sync"
	"time"
)

// Exec runs work with these guarantees (build-guide Task 13):
//   - decisions for ONE worker serialize (FIFO per worker id);
//   - decisions for DIFFERENT workers run concurrently;
//   - a global weighted semaphore caps total in-flight funcs.
//
// Concurrency invariant: the queue map AND each queue's pending/running fields
// are guarded by the SINGLE mutex `mu`, so map-membership and the running flag
// can never disagree (the earlier split-lock design could orphan a queue and run
// two drains for one worker — fixed). Funcs run OUTSIDE the lock.
type Exec struct {
	max    int
	sem    chan struct{}
	mu     sync.Mutex
	qs     map[string]*workerQueue
	closed bool
	wg     sync.WaitGroup
	rnd    *lockedRand
}

type workerQueue struct {
	pending []func()
	running bool
}

// NewExec builds an Exec allowing maxConcurrent funcs in flight at once.
func NewExec(maxConcurrent int) *Exec {
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	return &Exec{
		max: maxConcurrent,
		sem: make(chan struct{}, maxConcurrent),
		qs:  map[string]*workerQueue{},
		rnd: newLockedRand(),
	}
}

// Submit enqueues fn onto workerID's FIFO (no-op once Stopped). Funcs for the
// same worker run in submit order; different workers run concurrently.
func (x *Exec) Submit(workerID string, fn func()) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.closed {
		return
	}
	q := x.qs[workerID]
	if q == nil {
		q = &workerQueue{}
		x.qs[workerID] = q
	}
	q.pending = append(q.pending, fn)
	if !q.running {
		q.running = true
		x.wg.Add(1)
		go x.drain(workerID)
	}
}

func (x *Exec) drain(workerID string) {
	defer x.wg.Done()
	for {
		x.mu.Lock()
		q := x.qs[workerID]
		if q == nil || len(q.pending) == 0 {
			// Flip running=false and drop the empty queue atomically under mu, so
			// a concurrent Submit either sees running=true (and appends, we loop)
			// or sees the queue gone (and creates a fresh one) — never both.
			if q != nil {
				q.running = false
				delete(x.qs, workerID)
			}
			x.mu.Unlock()
			return
		}
		fn := q.pending[0]
		q.pending = q.pending[1:]
		x.mu.Unlock()

		x.runOne(fn)
	}
}

// runOne acquires a global slot and runs fn, recovering from a panic so a bug in
// off-write-path work can never crash the daemon (build-guide "never crash-loop").
func (x *Exec) runOne(fn func()) {
	x.sem <- struct{}{}
	defer func() {
		<-x.sem
		if r := recover(); r != nil {
			// Off-write-path work must not take down the process, but a swallowed
			// panic (e.g. in brainClassify) would silently stall a worker with no
			// trace — log it so the failure is diagnosable.
			log.Printf("arco: exec: recovered panic in off-write-path work: %v", r)
		}
	}()
	fn()
}

// Wait blocks until all currently-queued work has drained (tests/shutdown).
func (x *Exec) Wait() { x.wg.Wait() }

// Stop refuses new Submits and drains in-flight work. Call before closing the
// store so no off-write-path tx runs after Close.
func (x *Exec) Stop() {
	x.mu.Lock()
	x.closed = true
	x.mu.Unlock()
	x.wg.Wait()
}

// Backoff returns a decorrelated-jitter delay for retry attempt n (0-based),
// bounded by max. Never constant, never unbounded.
func (x *Exec) Backoff(attempt int, base, max time.Duration) time.Duration {
	if base <= 0 {
		base = 100 * time.Millisecond
	}
	if max <= 0 {
		max = 30 * time.Second
	}
	high := base << uint(minInt(attempt+1, 20))
	if high > max {
		high = max
	}
	if high <= base {
		return base
	}
	return base + time.Duration(x.rnd.int63n(int64(high-base)))
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// lockedRand is a tiny concurrency-safe PRNG for jitter (quality non-critical).
type lockedRand struct {
	mu sync.Mutex
	s  uint64
}

func newLockedRand() *lockedRand {
	return &lockedRand{s: uint64(time.Now().UnixNano()) | 1}
}

func (r *lockedRand) int63n(n int64) int64 {
	if n <= 0 {
		return 0
	}
	r.mu.Lock()
	r.s ^= r.s >> 12
	r.s ^= r.s << 25
	r.s ^= r.s >> 27
	v := (r.s * 0x2545F4914F6CDD1D) >> 1
	r.mu.Unlock()
	return int64(v) % n
}
