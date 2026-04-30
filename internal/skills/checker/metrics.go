package checker

// metrics.go registers Prometheus instrumentation for the skill upstream
// checker. The metrics are registered against the default Prometheus
// registry; if/when arc-relay grows a /metrics HTTP route (Task 15+) the
// existing scrape will already see these counters.
//
// Why singletons + init()? The checker Service may be constructed multiple
// times in tests (each subtest gets its own Service), but Prometheus panics
// on duplicate registration. Hoisting registration to package init() keeps
// it a one-shot global, which is the pattern Prometheus' own examples use.

import "github.com/prometheus/client_golang/prometheus"

var (
	checksTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "arc_relay_skill_checks_total",
		Help: "Total number of skill upstream checks, labeled by outcome (no_movement|no_path_touch|reverted_to_same|drift|error).",
	}, []string{"result"})

	checkDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "arc_relay_skill_check_duration_seconds",
		Help:    "Duration of per-skill upstream checks in seconds.",
		Buckets: prometheus.DefBuckets,
	})
)

func init() {
	prometheus.MustRegister(checksTotal, checkDuration)
}
