// Package metrics is glidepath-api's Prometheus surface, served on its own listener (METRICS_LISTEN, :9090) so a
// scrape needs no token and never reaches a /v1 route.
//
// One counter and one histogram per operation, labelled the way the audit line is (op, tier, code, decision), so a
// dashboard and the audit log answer the same question. The SLOs (the chart's glidepath-api PrometheusServiceLevel)
// read only these two: availability is the share of calls that did not answer 5xx; latency the share of the
// non-streaming calls that answered within LatencyThreshold. A call whose caller went away first is code 499 and
// counts against neither.
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// StatusClientClosed is what a call is counted as when its caller went away before it answered (nginx's 499).
const StatusClientClosed = 499

// LatencyThreshold is the latency SLO's bound; it must be one of the histogram's buckets.
const LatencyThreshold = 2.5

var buckets = []float64{0.05, 0.1, 0.25, 0.5, 1, LatencyThreshold, 5, 10, 30}

// Metrics is nil-safe: a Server without one (tests) records nothing.
type Metrics struct {
	reg      *prometheus.Registry
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
	inFlight *prometheus.GaugeVec
}

// Gauges a running server reads at scrape time.
type Gauges struct {
	LiveWatches  func() float64 // namespaces the live hub polls
	EventStreams func() float64
}

// New registers glidepath-api's metrics, the Go runtime's and the process's.
func New(contractVersion int, g Gauges) *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		reg: reg,
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "glidepath_api_requests_total",
			Help: "Calls answered, by operation (the contract's operationId), tier, HTTP status and authorization decision.",
		}, []string{"op", "tier", "code", "decision"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "glidepath_api_request_duration_seconds",
			Help:    "Time to answer a non-streaming call, by operation and tier. Streams (live, events) are not observed: they last as long as their viewer.",
			Buckets: buckets,
		}, []string{"op", "tier"}),
		inFlight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "glidepath_api_in_flight_requests",
			Help: "Calls being answered now, by operation; for a stream operation, the open streams.",
		}, []string{"op"}),
	}
	reg.MustRegister(m.requests, m.duration, m.inFlight,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	info := prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        "glidepath_api_contract_info",
		Help:        "The contractVersion this replica serves.",
		ConstLabels: prometheus.Labels{"contract_version": strconv.Itoa(contractVersion)},
	})
	info.Set(1)
	reg.MustRegister(info)
	if g.LiveWatches != nil {
		reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "glidepath_api_live_watches",
			Help: "Namespaces this replica's live hub is polling for its stream viewers.",
		}, g.LiveWatches))
	}
	if g.EventStreams != nil {
		reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "glidepath_api_event_streams",
			Help: "GET /v1/events streams this replica serves.",
		}, g.EventStreams))
	}
	return m
}

// Start counts a call in flight; the func it returns records how it ended.
func (m *Metrics) Start(op, tier string, stream bool) func(code int, decision string) {
	if m == nil {
		return func(int, string) {}
	}
	start := time.Now()
	g := m.inFlight.WithLabelValues(op)
	g.Inc()
	return func(code int, decision string) {
		g.Dec()
		m.requests.WithLabelValues(op, tier, strconv.Itoa(code), decision).Inc()
		if !stream && code != StatusClientClosed {
			m.duration.WithLabelValues(op, tier).Observe(time.Since(start).Seconds())
		}
	}
}

// Handler serves the registry in the Prometheus exposition format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{Registry: m.reg})
}
