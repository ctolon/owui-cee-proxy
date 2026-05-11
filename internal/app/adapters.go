package app

import (
	"time"

	"github.com/rs/zerolog"

	"github.com/ctolon/owui-cee-proxy/internal/engine/authutil"
	"github.com/ctolon/owui-cee-proxy/internal/observability"
	"github.com/ctolon/owui-cee-proxy/internal/transport/http/handlers"
)

// authMetricsAdapter bridges the authutil.AuthMetrics interface to
// the Prometheus counter. Lives at the composition root so the
// engine/authutil package stays Prometheus-agnostic.
type authMetricsAdapter struct{ m *observability.Metrics }

func (a *authMetricsAdapter) RecordAuthAttempt(engineName, scheme string, outcome authutil.Outcome) {
	if a.m == nil || a.m.EngineAuthAttached == nil {
		return
	}
	a.m.EngineAuthAttached.WithLabelValues(engineName, scheme, string(outcome)).Inc()
}

// ssrfMetricsAdapter bridges handlers.SSRFMetrics to the SSRFRejected
// + SSRFDNSCache Prometheus counters. The handlers package stays
// metric-agnostic; metric label values come from the typed
// SSRFRejectReason taxonomy.
type ssrfMetricsAdapter struct{ m *observability.Metrics }

func (s *ssrfMetricsAdapter) RecordRejection(reason handlers.SSRFRejectReason) {
	if s.m == nil || s.m.SSRFRejected == nil {
		return
	}
	s.m.SSRFRejected.WithLabelValues(string(reason)).Inc()
}

func (s *ssrfMetricsAdapter) RecordDNSCacheHit() {
	if s.m == nil || s.m.SSRFDNSCache == nil {
		return
	}
	s.m.SSRFDNSCache.WithLabelValues("hit").Inc()
}

func (s *ssrfMetricsAdapter) RecordDNSCacheMiss() {
	if s.m == nil || s.m.SSRFDNSCache == nil {
		return
	}
	s.m.SSRFDNSCache.WithLabelValues("miss").Inc()
}

func (s *ssrfMetricsAdapter) RecordDNSCacheBypass() {
	if s.m == nil || s.m.SSRFDNSCache == nil {
		return
	}
	s.m.SSRFDNSCache.WithLabelValues("bypass").Inc()
}

// ssrfLoggerAdapter writes a warn-level log line for every SSRF
// rejection. The raw URL is logged as-is (not redacted) because it
// is operator-facing audit data; the reason is what dashboards
// alert on, not the URL.
type ssrfLoggerAdapter struct{ logger zerolog.Logger }

func (l *ssrfLoggerAdapter) LogRejection(host, raw string, reason handlers.SSRFRejectReason, err error) {
	ev := l.logger.Warn().
		Str("event", "ssrf_rejected").
		Str("reason", string(reason)).
		Str("host", host).
		Str("url", raw)
	if err != nil {
		ev = ev.Err(err)
	}
	ev.Msg("SSRF policy rejected URL")
}

// taskLifecycleAdapter bridges tasks.LifecycleRecorder to the
// TaskLifecycle Prometheus counter.
type taskLifecycleAdapter struct{ m *observability.Metrics }

func (a *taskLifecycleAdapter) RecordTaskLifecycle(stage, outcome string) {
	if a.m == nil || a.m.TaskLifecycle == nil {
		return
	}
	a.m.TaskLifecycle.WithLabelValues(stage, outcome).Inc()
}

// queueDepthAdapter bridges tasks.QueueDepthRecorder to the
// WorkerQueueDepth gauge.
type queueDepthAdapter struct{ m *observability.Metrics }

func (a *queueDepthAdapter) SetQueueDepth(queue string, depth int) {
	if a.m == nil || a.m.WorkerQueueDepth == nil {
		return
	}
	a.m.WorkerQueueDepth.WithLabelValues(queue).Set(float64(depth))
}

// healthMetricsAdapter bridges handlers.HealthMetrics to the
// HealthProbeTotal + HealthProbeDuration collectors.
type healthMetricsAdapter struct{ m *observability.Metrics }

func (h *healthMetricsAdapter) RecordHealthProbe(engineName, status string, dur time.Duration) {
	if h.m == nil {
		return
	}
	if h.m.HealthProbeTotal != nil {
		h.m.HealthProbeTotal.WithLabelValues(engineName, status).Inc()
	}
	if h.m.HealthProbeDuration != nil {
		h.m.HealthProbeDuration.WithLabelValues(engineName).Observe(dur.Seconds())
	}
}

// panicRecorderAdapter bridges mw.PanicRecorder onto the
// PanicsTotal counter. Nil metrics → no-op; the Recover middleware
// still emits its structured log line.
type panicRecorderAdapter struct{ m *observability.Metrics }

func (p *panicRecorderAdapter) RecordPanic(path string) {
	if p.m == nil || p.m.PanicsTotal == nil {
		return
	}
	// chi's RoutePattern is preferable to the raw URL.Path but isn't
	// available from the panic site; we accept the raw path here.
	// Cardinality is bounded in practice by chi route patterns plus
	// the literal "unknown" for 404s.
	if path == "" {
		path = "unknown"
	}
	p.m.PanicsTotal.WithLabelValues(path).Inc()
}

// upstreamStatusAdapter bridges httpclient.UpstreamStatusRecorder to
// the EngineUpstreamStatus counter. Same rationale as above.
type upstreamStatusAdapter struct{ m *observability.Metrics }

func (u *upstreamStatusAdapter) RecordUpstreamStatus(engineName string, status int) {
	if u.m == nil || u.m.EngineUpstreamStatus == nil {
		return
	}
	u.m.EngineUpstreamStatus.WithLabelValues(engineName, observability.StatusClass(status)).Inc()
}
