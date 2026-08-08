// Guideline tests for T2.5: GET /metrics is a Prometheus scrape surface
// (promhttp) on the api mux. Fleet gauges (workers by state, pending
// escalations + oldest age) are computed at scrape time from the ledger
// reader; runtime instruments (brain calls/tokens, sweep duration, notify
// failures) are in-process counters the daemon increments. Each Metrics
// instance owns its OWN registry — tests build several servers in one process,
// so registration must not collide or panic.
package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/ledger"
	"github.com/dinhlongviolin1/arco/internal/reconcile"
	"github.com/dinhlongviolin1/arco/internal/vm"
)

func newMetricsAPI(t *testing.T) (*httptest.Server, *ledger.Store, *Metrics) {
	t.Helper()
	s, err := ledger.Open(t.TempDir() + "/api.db")
	require.NoError(t, err)
	require.NoError(t, s.Migrate(context.Background()))
	t.Cleanup(func() { s.Close() })
	srv := New(s, reconcile.New(s, vm.NewFake()))
	m := NewMetrics(s)
	srv.EnableMetrics(m)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, s, m
}

// scrape GETs /metrics and returns (body, content-type).
func scrape(t *testing.T, ts *httptest.Server) (string, string) {
	t.Helper()
	resp, err := http.Get(ts.URL + "/metrics")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(b), resp.Header.Get("Content-Type")
}

// metricValue finds the sample line for a series (name including any {labels})
// in Prometheus text exposition format and returns its value.
func metricValue(t *testing.T, body, series string) float64 {
	t.Helper()
	for _, ln := range strings.Split(body, "\n") {
		if strings.HasPrefix(ln, series+" ") {
			v, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(ln, series+" ")), 64)
			require.NoError(t, err, "unparsable sample line: %q", ln)
			return v
		}
	}
	t.Fatalf("series %q not found in scrape:\n%s", series, body)
	return 0
}

// Fleet gauges reflect seeded ledger state at scrape time.
func TestMetrics_ScrapeSeededFleet(t *testing.T) {
	ts, s, _ := newMetricsAPI(t)
	ctx := context.Background()

	session := ulid.Make().String()
	require.NoError(t, s.WithTx(ctx, func(tx core.Tx) error {
		return tx.CreateSession(core.Session{ID: session, Status: core.SessionOpen, Kind: core.SessionKindWork})
	}))

	// three workers: two running, one still starting
	ids := []string{ulid.Make().String(), ulid.Make().String(), ulid.Make().String()}
	for _, id := range ids {
		id := id
		require.NoError(t, s.WithTx(ctx, func(tx core.Tx) error {
			return tx.CreateWorker(core.Worker{ID: id, OwnerSession: session, State: core.WorkerStarting, Workspace: "arco_" + id})
		}))
	}
	for _, id := range ids[:2] {
		id := id
		require.NoError(t, s.WithTx(ctx, func(tx core.Tx) error {
			return tx.TransitionWorker(id, core.WorkerRunning, 0, core.Event{Kind: "state_change", WorkerID: id})
		}))
	}

	// one pending escalation, backdated 90s so the age gauge has a floor
	var escID string
	require.NoError(t, s.WithTx(ctx, func(tx core.Tx) error {
		var err error
		escID, err = tx.OpenEscalation(core.Escalation{WorkerID: ids[0], SessionID: session, Kind: "question", Action: "q?"})
		return err
	}))
	past := time.Now().Add(-90 * time.Second).UTC().Format(time.RFC3339Nano)
	_, err := s.DB().Exec(`UPDATE escalations SET requested_at=? WHERE id=?`, past, escID)
	require.NoError(t, err)

	body, ct := scrape(t, ts)
	require.Contains(t, ct, "text/plain", "prometheus text exposition format")

	require.Equal(t, float64(2), metricValue(t, body, `arco_workers{state="running"}`))
	require.Equal(t, float64(1), metricValue(t, body, `arco_workers{state="starting"}`))
	require.Equal(t, float64(1), metricValue(t, body, "arco_escalations_pending"))

	age := metricValue(t, body, "arco_escalation_oldest_age_seconds")
	require.GreaterOrEqual(t, age, float64(60), "backdated 90s → age at least 60s")
	require.Less(t, age, float64(3600), "sane, not garbage")
}

// Runtime instruments: brain calls/tokens, notify failures, sweep duration.
// Hammered from goroutines because the daemon increments them from the sweep
// loop, API handlers, and post-commit notify goroutines concurrently (-race).
func TestMetrics_RuntimeInstruments(t *testing.T) {
	ts, _, m := newMetricsAPI(t)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.BrainCall(20)
		}()
	}
	wg.Wait()
	m.NotifyFailure()
	m.NotifyFailure()
	m.NotifyFailure()
	m.SweepDone(1500 * time.Millisecond)

	body, _ := scrape(t, ts)
	require.Equal(t, float64(10), metricValue(t, body, "arco_brain_calls_total"))
	require.Equal(t, float64(200), metricValue(t, body, "arco_brain_tokens_total"))
	require.Equal(t, float64(3), metricValue(t, body, "arco_notify_failures_total"))
	require.InDelta(t, 1.5, metricValue(t, body, "arco_sweep_duration_seconds"), 0.001,
		"last completed sweep duration, in seconds")
}

// Zero state scrapes clean: counters registered (visible at 0), no pending
// escalations, age 0 — never a 500 or a panic on an empty ledger.
func TestMetrics_ZeroState(t *testing.T) {
	ts, _, _ := newMetricsAPI(t)
	body, _ := scrape(t, ts)
	require.Equal(t, float64(0), metricValue(t, body, "arco_brain_calls_total"))
	require.Equal(t, float64(0), metricValue(t, body, "arco_brain_tokens_total"))
	require.Equal(t, float64(0), metricValue(t, body, "arco_notify_failures_total"))
	require.Equal(t, float64(0), metricValue(t, body, "arco_escalations_pending"))
	require.Equal(t, float64(0), metricValue(t, body, "arco_escalation_oldest_age_seconds"))
}
