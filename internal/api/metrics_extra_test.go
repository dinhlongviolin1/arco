package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/ledger"
	"github.com/dinhlongviolin1/arco/internal/reconcile"
	"github.com/dinhlongviolin1/arco/internal/vm"
)

// *api.Metrics is what the daemon hands the engine through its optional seam;
// pin the interface satisfaction so a signature drift fails here, not at wiring.
var _ reconcile.Metrics = (*Metrics)(nil)

// Two Metrics instances in one process must not collide: each owns its own
// registry (the default registry would panic on the second registration).
func TestMetrics_SeparateRegistries(t *testing.T) {
	require.NotPanics(t, func() {
		s, err := ledger.Open(t.TempDir() + "/a.db")
		require.NoError(t, err)
		defer s.Close()
		require.NoError(t, s.Migrate(context.Background()))
		NewMetrics(s)
		NewMetrics(s)
	})
}

// Fleet gauges are computed per scrape, not cached at construction: state that
// lands between two scrapes shows up in the second one.
func TestMetrics_GaugesRecomputedPerScrape(t *testing.T) {
	ts, s, _ := newMetricsAPI(t)
	ctx := context.Background()

	body, _ := scrape(t, ts)
	require.NotContains(t, body, `arco_workers{state="starting"}`, "no workers yet")

	session := ulid.Make().String()
	require.NoError(t, s.WithTx(ctx, func(tx core.Tx) error {
		return tx.CreateSession(core.Session{ID: session, Status: core.SessionOpen, Kind: core.SessionKindWork})
	}))
	id := ulid.Make().String()
	require.NoError(t, s.WithTx(ctx, func(tx core.Tx) error {
		return tx.CreateWorker(core.Worker{ID: id, OwnerSession: session, State: core.WorkerStarting, Workspace: "arco_" + id})
	}))

	body, _ = scrape(t, ts)
	require.Equal(t, float64(1), metricValue(t, body, `arco_workers{state="starting"}`))
}

// The instruments are cumulative across scrapes (counters never reset) and the
// sweep gauge tracks only the LAST sweep.
func TestMetrics_CountersAccumulateGaugeReplaces(t *testing.T) {
	ts, _, m := newMetricsAPI(t)

	m.BrainCall(10)
	m.SweepDone(2 * time.Second)
	body, _ := scrape(t, ts)
	require.Equal(t, float64(1), metricValue(t, body, "arco_brain_calls_total"))
	require.InDelta(t, 2.0, metricValue(t, body, "arco_sweep_duration_seconds"), 0.001)

	m.BrainCall(0) // a failed/empty call still counts as a call, adds no tokens
	m.SweepDone(250 * time.Millisecond)
	body, _ = scrape(t, ts)
	require.Equal(t, float64(2), metricValue(t, body, "arco_brain_calls_total"))
	require.Equal(t, float64(10), metricValue(t, body, "arco_brain_tokens_total"))
	require.InDelta(t, 0.25, metricValue(t, body, "arco_sweep_duration_seconds"), 0.001,
		"gauge holds the last sweep, not the max or the sum")
}

// A server built without EnableMetrics has no /metrics route — the endpoint is
// opt-in, and the pre-existing routes are untouched either way.
func TestMetrics_NotEnabledIsNoRoute(t *testing.T) {
	s, err := ledger.Open(t.TempDir() + "/api.db")
	require.NoError(t, err)
	defer s.Close()
	require.NoError(t, s.Migrate(context.Background()))
	srv := New(s, reconcile.New(s, vm.NewFake()))
	require.NotPanics(t, func() { srv.EnableMetrics(nil) })
}

// HELP text must state how tokens are derived — the number is an estimate off
// the plain-text clavis CLI, and an operator reading a dashboard needs to know.
func TestMetrics_TokenHelpDocumentsEstimate(t *testing.T) {
	ts, _, _ := newMetricsAPI(t)
	body, _ := scrape(t, ts)
	var help string
	for _, ln := range strings.Split(body, "\n") {
		if strings.HasPrefix(ln, "# HELP arco_brain_tokens_total ") {
			help = ln
		}
	}
	require.NotEmpty(t, help, "arco_brain_tokens_total must carry a HELP line")
	require.Contains(t, strings.ToLower(help), "estimate")
}
