package api

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/dinhlongviolin1/arco/internal/core"
)

// Metrics is the Prometheus scrape surface (rev7/T2.5). It has two halves:
//
//   - runtime instruments — in-process counters/gauges the daemon and the
//     reconcile engine bump as work happens (brain calls/tokens, notify send
//     failures, last sweep duration). prometheus's own types are already
//     concurrency-safe, so these need no extra locking.
//   - fleet gauges — derived state (workers by state, pending escalations and
//     the oldest one's age) computed FROM THE LEDGER READER at scrape time by a
//     custom prometheus.Collector. No background poller: a gauge that is only a
//     projection of committed rows must never be able to go stale, and a
//     scraper that stops scraping should cost nothing.
//
// Each Metrics owns its OWN registry rather than using the global default one:
// a process may build several daemons/servers (tests do), and a duplicate
// registration on the shared default registry panics.
//
// Nothing derived from a prompt, a task, or a credential ever reaches a label
// or a value here — the only label in the whole surface is a worker state.
type Metrics struct {
	reg *prometheus.Registry

	brainCalls  prometheus.Counter
	brainTokens prometheus.Counter
	notifyFails prometheus.Counter
	sweepDur    prometheus.Gauge
}

// NewMetrics builds the registry, registers the runtime instruments, and wires
// a reader-backed collector for the fleet gauges over s.
func NewMetrics(s core.Store) *Metrics {
	m := &Metrics{
		reg: prometheus.NewRegistry(),
		brainCalls: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "arco_brain_calls_total",
			Help: "Total short-lived decision-brain invocations (classify + rollup).",
		}),
		brainTokens: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "arco_brain_tokens_total",
			// The brain is invoked by shelling out to `clavis ... -p <prompt>`,
			// which returns plain text — there is NO usage/billing surface to
			// read. This is therefore an APPROXIMATION: (bytes(prompt) +
			// bytes(response)) / 4, the usual ~4-bytes-per-token rule of thumb.
			// Treat it as a trend line for prompt-budget work, never as a bill.
			Help: "Approximate brain tokens consumed: (prompt+response bytes)/4. " +
				"The clavis CLI path returns no usage data, so this is an estimate, not billing truth.",
		}),
		notifyFails: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "arco_notify_failures_total",
			Help: "Decision-card push sends that failed (logged and swallowed; a notify outage never fails a reconcile).",
		}),
		sweepDur: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "arco_sweep_duration_seconds",
			Help: "Wall-clock duration of the LAST completed reconcile sweep, in seconds.",
		}),
	}
	m.reg.MustRegister(m.brainCalls, m.brainTokens, m.notifyFails, m.sweepDur, &fleetCollector{store: s})
	return m
}

// BrainCall records one brain invocation and its (approximate) token cost.
func (m *Metrics) BrainCall(tokens int) {
	if m == nil {
		return
	}
	m.brainCalls.Inc()
	if tokens > 0 {
		m.brainTokens.Add(float64(tokens))
	}
}

// NotifyFailure records one failed decision-card push.
func (m *Metrics) NotifyFailure() {
	if m == nil {
		return
	}
	m.notifyFails.Inc()
}

// SweepDone records how long the sweep that just finished took. It is a gauge,
// not a histogram: the operator question this answers is "is the current sweep
// loop keeping up", which is about the latest tick, not a distribution.
func (m *Metrics) SweepDone(d time.Duration) {
	if m == nil {
		return
	}
	m.sweepDur.Set(d.Seconds())
}

// handler serves this registry in Prometheus text exposition format.
func (m *Metrics) handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

// EnableMetrics registers GET /metrics on the existing mux. Call once, before
// serving (http.ServeMux panics on a duplicate pattern); a nil Metrics is a
// no-op, so a caller that did not build one simply has no /metrics route.
func (s *Server) EnableMetrics(m *Metrics) {
	if m == nil {
		return
	}
	s.mux.Handle("GET /metrics", m.handler())
}

// ---- fleet gauges (computed at scrape time from the ledger) -----------------

var (
	workersDesc = prometheus.NewDesc(
		"arco_workers",
		"Current workers by lifecycle state.",
		[]string{"state"}, nil,
	)
	escalationsPendingDesc = prometheus.NewDesc(
		"arco_escalations_pending",
		"Escalations currently awaiting an operator decision.",
		nil, nil,
	)
	escalationOldestAgeDesc = prometheus.NewDesc(
		"arco_escalation_oldest_age_seconds",
		"Age of the OLDEST pending escalation, in seconds (0 when none are pending).",
		nil, nil,
	)
)

// fleetCollector projects committed ledger rows into gauges on every scrape.
// Read errors are absorbed into "no series" rather than failing the scrape: a
// transient ledger read must not take /metrics (and any alerting hanging off
// the runtime instruments) down with it.
type fleetCollector struct{ store core.Store }

func (c *fleetCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- workersDesc
	ch <- escalationsPendingDesc
	ch <- escalationOldestAgeDesc
}

func (c *fleetCollector) Collect(ch chan<- prometheus.Metric) {
	r := c.store.Reader()

	if ws, err := r.ListWorkers(core.WorkerFilter{}); err == nil {
		byState := map[string]int{}
		for _, w := range ws {
			byState[string(w.State)]++
		}
		for state, n := range byState {
			ch <- prometheus.MustNewConstMetric(workersDesc, prometheus.GaugeValue, float64(n), state)
		}
	}

	// Pending escalations always report, even at zero — an alert on "pending
	// backlog cleared" needs the 0, not a vanished series.
	es, err := r.ListEscalations(core.EscalationFilter{Status: "pending"})
	if err != nil {
		es = nil
	}
	var oldest int64
	now := time.Now()
	for _, e := range es {
		// ageSeconds (server.go) is the shared contract: RFC3339Nano in,
		// non-negative seconds out, 0 on an unparsable timestamp.
		if age := ageSeconds(e.RequestedAt, now); age > oldest {
			oldest = age
		}
	}
	ch <- prometheus.MustNewConstMetric(escalationsPendingDesc, prometheus.GaugeValue, float64(len(es)))
	ch <- prometheus.MustNewConstMetric(escalationOldestAgeDesc, prometheus.GaugeValue, float64(oldest))
}
