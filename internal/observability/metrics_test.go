package observability

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/stretchr/testify/require"
)

func TestStatusClass_Table(t *testing.T) {
	t.Parallel()
	cases := []struct {
		code int
		want string
	}{
		{100, "1xx"},
		{199, "1xx"},
		{200, "2xx"},
		{204, "2xx"},
		{301, "3xx"},
		{400, "4xx"},
		{422, "4xx"},
		{499, "4xx"},
		{500, "5xx"},
		{599, "5xx"},
		{0, "error"},
		{-1, "error"},
		{600, "error"},
		{1000, "error"},
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, StatusClass(tc.code), "StatusClass(%d)", tc.code)
	}
}

// TestNewMetrics_AllCollectorsRegister stands up the metrics struct
// and scrapes /metrics from a httptest server backed by the same
// registry. The assertion is "no Prometheus collector double-
// registered or had an invalid label set" — both are silent failures
// at boot that this test pins down.
func TestNewMetrics_AllCollectorsRegister(t *testing.T) {
	t.Parallel()
	m := NewMetrics()
	require.NotNil(t, m.Registry)

	// Bump one label cell on each collector so they appear in the
	// scrape output (Prometheus omits empty vectors).
	m.RequestsTotal.WithLabelValues("docling", "/v1/convert/file", "POST", "2xx").Inc()
	m.RequestDuration.WithLabelValues("docling", "/v1/convert/file", "POST").Observe(0.1)
	m.InFlight.WithLabelValues("docling").Inc()
	m.UpstreamErrors.WithLabelValues("docling", "5xx").Inc()
	m.BreakerStateGauge.WithLabelValues("docling").Set(0)
	m.BodyBytes.WithLabelValues("docling", "in").Observe(1024)
	m.EngineAuthAttached.WithLabelValues("docling", "raw", "attached").Inc()
	m.EngineUpstreamStatus.WithLabelValues("docling", "2xx").Inc()
	m.SSRFRejected.WithLabelValues("metadata_ip").Inc()
	m.SSRFDNSCache.WithLabelValues("hit").Inc()
	m.HealthProbeTotal.WithLabelValues("docling", "ok").Inc()
	m.HealthProbeDuration.WithLabelValues("docling").Observe(0.01)
	m.WorkerQueueDepth.WithLabelValues("default").Set(3)
	m.TaskLifecycle.WithLabelValues("enqueued", "success").Inc()

	srv := httptest.NewServer(promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	buf := make([]byte, 64*1024)
	n, _ := resp.Body.Read(buf)
	out := string(buf[:n])

	// Every collector we declared must surface in the scrape so
	// dashboards can rely on the namespaces.
	required := []string{
		"owui_cee_proxy_requests_total",
		"owui_cee_proxy_request_duration_seconds",
		"owui_cee_proxy_inflight_requests",
		"owui_cee_proxy_upstream_errors_total",
		"owui_cee_proxy_breaker_state",
		"owui_cee_proxy_body_bytes",
		"owui_cee_proxy_engine_auth_attached_total",
		"owui_cee_proxy_engine_upstream_status_total",
		"owui_cee_proxy_ssrf_rejected_total",
		"owui_cee_proxy_ssrf_dns_cache_total",
		"owui_cee_proxy_health_probe_total",
		"owui_cee_proxy_health_probe_duration_seconds",
		"owui_cee_proxy_worker_queue_depth",
		"owui_cee_proxy_task_lifecycle_total",
	}
	for _, want := range required {
		require.True(t, strings.Contains(out, want),
			"collector %s not present in /metrics scrape", want)
	}
}
