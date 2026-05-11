package httpclient

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	otelsdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/ctolon/owui-cee-proxy/internal/config"
)

func TestNew_Defaults(t *testing.T) {
	t.Parallel()
	c, err := New(config.EngineConfig{
		URL: "http://example:8080",
	})
	require.NoError(t, err)
	require.NotNil(t, c.HTTP)
	require.Equal(t, 120*time.Second, c.RequestTimeout)
	require.Equal(t, "http://example:8080", c.BaseURL)
}

func TestNew_TLSPartialCertReturnsError(t *testing.T) {
	t.Parallel()
	_, err := New(config.EngineConfig{
		URL: "https://example:8080",
		HTTP: config.HTTPClientConfig{
			TLS: config.TLSClientConfig{CertFile: "/nope.pem"},
		},
	})
	require.Error(t, err)
}

func TestNew_ResponseHeaderTimeoutDefaultsToRequestTimeout(t *testing.T) {
	t.Parallel()
	// M7: when ResponseHeaderTimeout is unset (zero) we want it to
	// default to the configured RequestTimeout, not the legacy 30s,
	// so large-PDF backends that take >30s to emit headers don't
	// trip a transport-level timeout before the engine starts
	// streaming.
	c, err := New(config.EngineConfig{
		URL:            "http://example:8080",
		RequestTimeout: 4 * time.Minute,
	})
	require.NoError(t, err)
	tr := c.InnerTransport()
	require.NotNil(t, tr, "InnerTransport must expose the raw *http.Transport under the OTel/instrumented wraps")
	require.Equal(t, 4*time.Minute, tr.ResponseHeaderTimeout)
}

func TestNew_ResponseHeaderTimeoutExplicitWins(t *testing.T) {
	t.Parallel()
	c, err := New(config.EngineConfig{
		URL:            "http://example:8080",
		RequestTimeout: 4 * time.Minute,
		HTTP: config.HTTPClientConfig{
			ResponseHeaderTimeout: 7 * time.Second,
		},
	})
	require.NoError(t, err)
	tr := c.InnerTransport()
	require.NotNil(t, tr, "InnerTransport must expose the raw *http.Transport under the OTel/instrumented wraps")
	require.Equal(t, 7*time.Second, tr.ResponseHeaderTimeout)
}

// TestNew_OtelHTTPTransportPropagatesTraceparent pins C-19: outbound
// requests carry the W3C `traceparent` header so the engine backend
// can continue the inbound span tree. The otelhttp.NewTransport wrap
// reads the active span off ctx and stamps the header automatically.
//
// Build a tiny upstream that records the incoming traceparent. With
// an in-memory tracer providing the span, the assertion is: the
// header is present AND its trace-id matches the active span's.
func TestNew_OtelHTTPTransportPropagatesTraceparent(t *testing.T) {
	t.Parallel()
	var seenTP string
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seenTP = r.Header.Get("traceparent")
	}))
	defer srv.Close()

	c, err := New(config.EngineConfig{
		URL:            srv.URL,
		RequestTimeout: 5 * time.Second,
	})
	require.NoError(t, err)

	tp := otelsdktrace.NewTracerProvider(otelsdktrace.WithSyncer(tracetest.NewInMemoryExporter()))
	defer func() { _ = tp.Shutdown(t.Context()) }()
	tracer := tp.Tracer("test")
	ctx, span := tracer.Start(t.Context(), "outbound")
	defer span.End()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	require.NoError(t, err)
	resp, err := c.HTTP.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()

	require.NotEmpty(t, seenTP, "outbound request MUST carry traceparent — otelhttp wrap did not fire")
	wantTraceID := span.SpanContext().TraceID().String()
	require.Contains(t, seenTP, wantTraceID,
		"traceparent MUST carry the active span's trace_id so the backend continues the tree")
}
