package metrics

import (
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
)

// Metrics holds all gateway Prometheus metrics.
type Metrics struct {
	FanoutRequests   *prometheus.CounterVec
	SuppressedErrors *prometheus.CounterVec
	PartialFailures  *prometheus.CounterVec
}

// New creates and registers all gateway metrics with reg.
// Pass prometheus.DefaultRegisterer in production; prometheus.NewRegistry() in tests.
func New(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		FanoutRequests: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "gateway_fanout_requests_total",
				Help: "Fan-out push attempts, labeled by instance, target, status.",
			},
			[]string{"instance", "target", "status"},
		),
		SuppressedErrors: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "gateway_suppressed_errors_total",
				Help: "Mimir 400 responses suppressed in any mode, labeled by instance, target, pattern.",
			},
			[]string{"instance", "target", "pattern"},
		),
		PartialFailures: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "gateway_partial_failures_total",
				Help: "Pushes that returned success with X-Gateway-Partial-Failure header, labeled by instance.",
			},
			[]string{"instance"},
		),
	}
	reg.MustRegister(m.FanoutRequests, m.SuppressedErrors, m.PartialFailures)
	return m
}

func (m *Metrics) RecordFanout(instance, target string, status int) {
	m.FanoutRequests.WithLabelValues(instance, target, strconv.Itoa(status)).Inc()
}

func (m *Metrics) RecordSuppressed(instance, target, pattern string) {
	m.SuppressedErrors.WithLabelValues(instance, target, pattern).Inc()
}

func (m *Metrics) RecordPartialFailure(instance string) {
	m.PartialFailures.WithLabelValues(instance).Inc()
}
