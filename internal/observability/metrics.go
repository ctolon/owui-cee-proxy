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
	// EngineAuthAttached records every ApplyAuth decision. Labels:
	//   engine  — engines.<name> key
	//   scheme  — "raw" | "bearer" | "" (when no header configured)
	//   outcome — "attached"    : header stamped with a non-empty value
	//             "missing_key" : auth_header set but APIKey resolved empty
	//             "no_auth"     : neither header nor key configured
	// Operators alert on missing_key>0 to catch silent 401 setups
	// where the env var didn't propagate to the Pod.
	EngineAuthAttached *prometheus.CounterVec
	// EngineUpstreamStatus counts backend responses bucketed by HTTP
	// status CLASS (2xx/3xx/4xx/5xx/1xx/error — see StatusClass), per
	// engine. Bucketing keeps cardinality bounded; raw integer codes
	// would produce 500+ time series per engine under sustained
	// traffic. Distinct from UpstreamErrors: this one tracks ALL
	// statuses (incl. 2xx) so dashboards can compute error ratios.
	EngineUpstreamStatus *prometheus.CounterVec
	// SSRFRejected counts /v1/convert/source URLs the proxy refused
	// to dereference, broken down by reason. Labels:
	//   reason — resolution_failed | blocked_cidr | loopback |
	//            link_local | metadata_ip | multicast | non_http_scheme
	// Alert when reason="metadata_ip">0: someone is actively probing
	// for cloud-credential exfiltration.
	SSRFRejected *prometheus.CounterVec
	// SSRFDNSCache tracks LRU cache hits/misses for the resolver.
	// Operators tune Security.SSRF.DNSCacheTTL based on hit ratio.
	SSRFDNSCache *prometheus.CounterVec
	// HealthProbeTotal counts /readyz per-engine probe outcomes.
	// Labels: engine, status="ok"|"unhealthy". Pair with the
	// duration histogram below for RED-style dashboards.
	HealthProbeTotal *prometheus.CounterVec
	// HealthProbeDuration measures per-engine Health() wall-clock
	// time. A backend whose health probes routinely take >1s is
	// either GC-bound or under load; this metric surfaces it before
	// the kubelet probe timeout fires.
	HealthProbeDuration *prometheus.HistogramVec
	// WorkerQueueDepth is the gauge polled from asynq.Inspector.
	// Labels: queue (the asynq queue name). Spikes here precede
	// SLO violations on async file conversions.
	WorkerQueueDepth *prometheus.GaugeVec
	// TaskLifecycle is the per-stage counter for the async path.
	// Labels:
	//   stage   — enqueued | dequeued | completed | failed
	//   outcome — success | error | cancelled | retry
	// Stage=enqueued/dequeued always pair with outcome=success;
	// failed pairs with the error class. Use this to compute
	// queue → finish funnel attrition rates.
	TaskLifecycle *prometheus.CounterVec
}

// StatusClass buckets an HTTP status integer into a low-cardinality
// label value. Codes outside the standard 100-599 range collapse to
// "error". Used by both the inbound access-log metric path and the
// outbound upstream-status counter so dashboards have one schema.
//
// Operators querying historical 4xx rates do so via
// `rate(owui_cee_proxy_requests_total{status="4xx"}[5m])` — never
// by integer code, which is intentionally absent.
func StatusClass(code int) string {
	switch {
	case code >= 100 && code < 200:
		return "1xx"
	case code >= 200 && code < 300:
		return "2xx"
	case code >= 300 && code < 400:
		return "3xx"
	case code >= 400 && code < 500:
		return "4xx"
	case code >= 500 && code < 600:
		return "5xx"
	default:
		// 0 from transport errors (no response); negative or out-of-band
		// from misuse. Both worth surfacing distinctly from 5xx.
		return "error"
	}
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
		EngineAuthAttached: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: "owui_cee_proxy",
			Name:      "engine_auth_attached_total",
			Help:      "Outbound auth-stamping decisions made by ApplyAuth, per engine, scheme, and outcome.",
		}, []string{"engine", "scheme", "outcome"}),
		EngineUpstreamStatus: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: "owui_cee_proxy",
			Name:      "engine_upstream_status_total",
			Help:      "HTTP status class (2xx/4xx/5xx/error) returned by engine backends, per engine.",
		}, []string{"engine", "status"}),
		SSRFRejected: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: "owui_cee_proxy",
			Name:      "ssrf_rejected_total",
			Help:      "External source URLs the SSRF policy refused to dereference, per reason.",
		}, []string{"reason"}),
		SSRFDNSCache: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: "owui_cee_proxy",
			Name:      "ssrf_dns_cache_total",
			Help:      "SSRF resolver DNS cache outcomes: hit | miss | bypass.",
		}, []string{"result"}),
		HealthProbeTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: "owui_cee_proxy",
			Name:      "health_probe_total",
			Help:      "Readiness probe outcomes per engine.",
		}, []string{"engine", "status"}),
		HealthProbeDuration: factory.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "owui_cee_proxy",
			Name:      "health_probe_duration_seconds",
			Help:      "Wall-clock duration of per-engine readiness probes.",
			Buckets:   prometheus.DefBuckets,
		}, []string{"engine"}),
		WorkerQueueDepth: factory.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "owui_cee_proxy",
			Name:      "worker_queue_depth",
			Help:      "Number of pending tasks in each asynq queue (polled via Inspector).",
		}, []string{"queue"}),
		TaskLifecycle: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: "owui_cee_proxy",
			Name:      "task_lifecycle_total",
			Help:      "Async task lifecycle transitions per stage and outcome.",
		}, []string{"stage", "outcome"}),
	}
}
