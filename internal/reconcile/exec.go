package reconcile

import (
	"sync"
	"time"
)

// Exec runs work with two guarantees (build-guide Task 13):
//   - decisions for ONE worker serialize (FIFO per worker id), so a worker's
//     state never races itself;
//   - decisions for DIFFERENT workers run concurrently;
//   - a global weighted semaphore caps total in-flight funcs (spawns / brain
//     calls) so a burst can't exhaust the box or a provider.
//
// Submit is non-blocking: it enqueues onto the worker's FIFO and returns. The
// per-worker goroutine drains the queue in order, acquiring the global slot
// around each func. This keeps the brain OFF the single write path.
type Exec struct {
	max  int
	sem  chan struct{}
	mu   sync.Mutex
	qs   map[string]*workerQueue
	wg   sync.WaitGroup
	rnd  *lockedRand
	done chan struct{}
}

type workerQueue struct {
	mu      sync.Mutex
	pending []func()
	running bool
}

// NewExec builds an Exec allowing maxConcurrent funcs in flight at once.
func NewExec(maxConcurrent int) *Exec {
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	return &Exec{
		max:  maxConcurrent,
		sem:  make(chan struct{}, maxConcurrent),
		qs:   map[string]*workerQueue{},
		rnd:  newLockedRand(),
		done: make(chan struct{}),
	}
}

// Submit enqueues fn onto workerID's FIFO. Funcs for the same worker run in
// submit order; funcs for different workers run concurrently (subject to the
// global cap). Returns immediately.
func (x *Exec) Submit(workerID string, fn func()) {
	x.mu.Lock()
	q := x.qs[workerID]
	if q == nil {
		q = &workerQueue{}
		x.qs[workerID] = q
	}
	x.mu.Unlock()

	q.mu.Lock()
	q.pending = append(q.pending, fn)
	if !q.running {
		q.running = true
		x.wg.Add(1)
		go x.drain(workerID, q)
	}
	q.mu.Unlock()
}

func (x *Exec) drain(workerID string, q *workerQueue) {
	defer x.wg.Done()
	for {
		q.mu.Lock()
		if len(q.pending) == 0 {
			q.running = false
			q.mu.Unlock()
			// GC the empty queue so long-lived Execs don't leak per-worker maps.
			x.mu.Lock()
			if cur := x.qs[workerID]; cur == q {
				cur.mu.Lock()
				if len(cur.pending) == 0 && !cur.running {
					delete(x.qs, workerID)
				}
				cur.mu.Unlock()
			}
			x.mu.Unlock()
			return
		}
		fn := q.pending[0]
		q.pending = q.pending[1:]
		q.mu.Unlock()

		x.sem <- struct{}{} // acquire a global slot
		func() {
			defer func() { <-x.sem }()
			fn()
		}()
	}
}

// Wait blocks until all currently-queued work has drained. For tests/shutdown.
func (x *Exec) Wait() { x.wg.Wait() }

// Backoff returns a decorrelated-jitter delay for retry attempt n (0-based),
// bounded by cap. Never constant, never unbounded (build-guide 429 handling).
func (x *Exec) Backoff(attempt int, base, max time.Duration) time.Duration {
	if base <= 0 {
		base = 100 * time.Millisecond
	}
	if max <= 0 {
		max = 30 * time.Second
	}
	// decorrelated jitter: sleep = min(max, rand(base, prev*3)); approximate prev
	// as base*2^attempt, capped.
	high := base << uint(min(attempt+1, 20))
	if high > max {
		high = max
	}
	if high <= base {
		return base
	}
	return base + time.Duration(x.rnd.int63n(int64(high-base)))
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// lockedRand is a tiny concurrency-safe PRNG (math/rand/v2 is per-call safe, but
// we keep an explicit type to make intent clear and avoid global-seed reliance).
type lockedRand struct {
	mu sync.Mutex
	s  uint64
}

func newLockedRand() *lockedRand {
	// seed from a monotonic-ish source without importing time-of-day randomness
	// into hot paths; jitter quality is non-critical.
	return &lockedRand{s: uint64(time.Now().UnixNano()) | 1}
}

func (r *lockedRand) int63n(n int64) int64 {
	if n <= 0 {
		return 0
	}
	r.mu.Lock()
	// xorshift64*
	r.s ^= r.s >> 12
	r.s ^= r.s << 25
	r.s ^= r.s >> 27
	v := (r.s * 0x2545F4914F6CDD1D) >> 1
	r.mu.Unlock()
	return int64(v) % n
}
