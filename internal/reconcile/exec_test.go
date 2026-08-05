package reconcile

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Same-worker submits run strictly in order (serialized). Under -race, a missing
// serialization would also trip the detector on the unsynchronized slice.
func TestExec_SameWorkerSerializes(t *testing.T) {
	x := NewExec(8)
	var got []int
	const n = 100
	for i := 0; i < n; i++ {
		i := i
		x.Submit("w1", func() { got = append(got, i) }) // no lock: safe only if serialized
	}
	x.Wait()
	require.Len(t, got, n)
	for i := 0; i < n; i++ {
		require.Equal(t, i, got[i], "same-worker funcs must run in submit order")
	}
}

// Different workers run concurrently: 4 funcs on 4 workers each block until all
// 4 are simultaneously in flight. If they were serialized this would deadlock.
func TestExec_DifferentWorkersConcurrent(t *testing.T) {
	x := NewExec(4)
	const n = 4
	var active int32
	all := make(chan struct{})
	release := make(chan struct{})
	for i := 0; i < n; i++ {
		id := string(rune('a' + i))
		x.Submit(id, func() {
			if atomic.AddInt32(&active, 1) == n {
				close(all) // the last one to arrive proves all n are concurrent
			}
			<-release
		})
	}
	select {
	case <-all:
	case <-time.After(2 * time.Second):
		t.Fatal("workers did not run concurrently (serialized?)")
	}
	close(release)
	x.Wait()
}

// The global semaphore caps total in-flight funcs regardless of worker count.
func TestExec_GlobalCapBounded(t *testing.T) {
	const cap = 2
	x := NewExec(cap)
	var cur, peak int32
	var mu sync.Mutex
	for i := 0; i < 12; i++ {
		id := string(rune('a' + i))
		x.Submit(id, func() {
			c := atomic.AddInt32(&cur, 1)
			mu.Lock()
			if c > peak {
				peak = c
			}
			mu.Unlock()
			time.Sleep(10 * time.Millisecond)
			atomic.AddInt32(&cur, -1)
		})
	}
	x.Wait()
	require.LessOrEqual(t, int(peak), cap, "in-flight funcs must not exceed the global cap")
}

func TestExec_BackoffBoundedAndJittered(t *testing.T) {
	x := NewExec(1)
	base, max := 100*time.Millisecond, 5*time.Second
	seen := map[time.Duration]bool{}
	for attempt := 0; attempt < 8; attempt++ {
		for r := 0; r < 5; r++ {
			d := x.Backoff(attempt, base, max)
			require.GreaterOrEqual(t, d, base)
			require.LessOrEqual(t, d, max)
			seen[d] = true
		}
	}
	require.Greater(t, len(seen), 1, "backoff must be jittered, not constant")
}
