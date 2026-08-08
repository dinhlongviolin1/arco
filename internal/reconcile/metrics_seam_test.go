package reconcile

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/notify"
)

// countingMetrics is a race-safe fake for the optional Metrics seam.
type countingMetrics struct {
	mu           sync.Mutex
	calls        int
	tokens       int
	notifyFailed int
}

func (m *countingMetrics) BrainCall(tokens int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	m.tokens += tokens
}

func (m *countingMetrics) NotifyFailure() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.notifyFailed++
}

func (m *countingMetrics) snapshot() (calls, tokens, failed int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls, m.tokens, m.notifyFailed
}

// failingSender fails every send, like an unreachable push endpoint.
type failingSender struct{}

func (failingSender) Send(context.Context, notify.Card) error { return errors.New("boom") }

// A send failure is counted on the seam (and still swallowed: the reconcile
// path succeeds regardless).
func TestMetricsSeam_NotifyFailureCounted(t *testing.T) {
	e, s, _ := newEngine(t)
	m := &countingMetrics{}
	e.Metrics = m
	e.Notify = failingSender{}
	id := mkRunning(t, e, s, "/wt/m1", "base")

	require.NoError(t, e.ApplyEvent(context.Background(), EventInput{
		WorkerID: id, Alive: true, WaitingInput: true,
	}))

	// The push is post-commit and async, so poll rather than assume ordering.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, _, failed := m.snapshot(); failed >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for a counted notify failure")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The seam is OPTIONAL: an Engine built without it (every pre-existing caller)
// takes the same paths with no nil deref.
func TestMetricsSeam_NilIsNoOp(t *testing.T) {
	e, s, _ := newEngine(t)
	require.Nil(t, e.Metrics, "default Engine has no metrics sink")
	e.Notify = failingSender{}
	id := mkRunning(t, e, s, "/wt/m2", "base")

	require.NoError(t, e.ApplyEvent(context.Background(), EventInput{
		WorkerID: id, Alive: true, WaitingInput: true,
	}))
	e.meterBrainCall(42)
	e.meterNotifyFailure()
	time.Sleep(50 * time.Millisecond) // let the async send goroutine run
}

// approxTokens is the documented (prompt+response)/4 estimate — the clavis CLI
// exposes no usage data, so this is the whole of the token signal.
func TestApproxTokens(t *testing.T) {
	require.Equal(t, 0, approxTokens("", ""))
	require.Equal(t, 2, approxTokens("abcd", "efgh"))
	require.Equal(t, 3, approxTokens("abcdefghij", "xx"))
}
