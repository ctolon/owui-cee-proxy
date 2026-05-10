package observability

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics holds the project's Prometheus collectors. All collectors
// carry an `engine` label so dashboards can split RED metrics per
// extraction backend.
type Metrics struct {
	Registry          *prometheus.Registry
	RequestsTotal     *prometheus.CounterVec
	RequestDuration   *prometheus.HistogramVec
	InFlight          *prometheus.GaugeVec
	UpstreamErrors    *prometheus.CounterVec
	BreakerStateGauge *prometheus.GaugeVec
	BodyBytes         *prometheus.HistogramVec
}

func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	factory := promauto.With(reg)

	reg.MustRegister(
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		collectors.NewGoCollector(),
	)

	return &Metrics{
		Registry: reg,
		RequestsTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: "owui_cee_proxy",
			Name:      "requests_total",
			Help:      "Total number of HTTP requests handled by the proxy.",
		}, []string{"engine", "route", "method", "status"}),
		RequestDuration: factory.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "owui_cee_proxy",
			Name:      "request_duration_seconds",
			Help:      "Wall-clock duration of HTTP requests handled by the proxy.",
			Buckets:   prometheus.DefBuckets,
		}, []string{"engine", "route", "method"}),
		InFlight: factory.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "owui_cee_proxy",
			Name:      "inflight_requests",
			Help:      "Number of in-flight HTTP requests per engine.",
		}, []string{"engine"}),
		UpstreamErrors: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: "owui_cee_proxy",
			Name:      "upstream_errors_total",
			Help:      "Total number of upstream errors per engine and reason.",
		}, []string{"engine", "reason"}),
		BreakerStateGauge: factory.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "owui_cee_proxy",
			Name:      "breaker_state",
			Help:      "Circuit breaker state per engine: 0=closed, 1=half-open, 2=open.",
		}, []string{"engine"}),
		BodyBytes: factory.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "owui_cee_proxy",
			Name:      "body_bytes",
			Help:      "Bytes transferred in request/response bodies.",
			Buckets: []float64{
				1024, 4096, 16384, 65536, 262144, 1048576, 4194304, 16777216, 67108864, 268435456,
			},
		}, []string{"engine", "direction"}),
	}
}
