//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/ctolon/owui-cee-proxy/internal/breaker"
	"github.com/ctolon/owui-cee-proxy/internal/config"
	"github.com/ctolon/owui-cee-proxy/internal/engine"
	"github.com/ctolon/owui-cee-proxy/internal/engine/authutil"
	"github.com/ctolon/owui-cee-proxy/internal/engine/compat/docling"
	"github.com/ctolon/owui-cee-proxy/internal/engine/compat/doclingexternal"
	"github.com/ctolon/owui-cee-proxy/internal/engine/compat/external"
	"github.com/ctolon/owui-cee-proxy/internal/engine/compat/tika"
	"github.com/ctolon/owui-cee-proxy/internal/httpclient"
	"github.com/ctolon/owui-cee-proxy/internal/observability"
	httptransport "github.com/ctolon/owui-cee-proxy/internal/transport/http"
)

func newProxyServer(t *testing.T, cfg *config.Config) *httptest.Server {
	srv, _ := newProxyServerWithLogCapture(t, cfg)
	return srv
}

// logCapture is a thread-safe sink for proxy log lines used by the
// auth/upstream-status integration suite. The proxy emits structured
// JSON to its zerolog.Logger; we wire a parallel logger that mirrors
// every Write into a bytes.Buffer the test can grep through. The
// production NewLogger() is unused here so we can avoid touching
// zerolog's package-global Time/Field overrides during tests.
type logCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *logCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *logCapture) Bytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.buf.Bytes()...)
}

// findEvent scans c for the first log line whose "event" field
// matches name and returns it as a decoded map. Returns nil when no
// such line exists. Tests use this to assert observability fields
// (engine_url, auth_key_present, upstream_status, body_snippet, …)
// without coupling to the line ordering.
func (c *logCapture) findEvent(name string) map[string]any {
	for _, line := range bytes.Split(c.Bytes(), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			continue
		}
		if ev, _ := m["event"].(string); ev == name {
			return m
		}
	}
	return nil
}

// findAllEvents returns every event log line whose "event" matches.
// Useful when the same event fires multiple times (e.g., one
// outbound_engine_request per file in a batched call).
func (c *logCapture) findAllEvents(name string) []map[string]any {
	out := []map[string]any{}
	for _, line := range bytes.Split(c.Bytes(), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			continue
		}
		if ev, _ := m["event"].(string); ev == name {
			out = append(out, m)
		}
	}
	return out
}

// newProxyServerWithLogCapture builds the proxy with a parallel
// zerolog.Logger that writes to a returned logCapture sink. Use this
// when the test needs to assert observability fields (auth header
// stamping, upstream status, engine_url, …). Same as
// newProxyServer for everything else.
func newProxyServerWithLogCapture(t *testing.T, cfg *config.Config) (*httptest.Server, *logCapture) {
	t.Helper()

	cap := &logCapture{}
	// Build a debug-level logger that mirrors INTO the capture. We
	// intentionally avoid observability.NewLogger so its package-
	// global zerolog state mutations don't bleed across parallel
	// tests; the production wiring is exercised by unit tests.
	logger := zerolog.New(io.MultiWriter(io.Discard, cap)).
		Level(zerolog.DebugLevel).
		With().
		Timestamp().
		Logger()
	metrics := observability.NewMetrics()

	authM := &captureAuthMetrics{m: metrics}
	upstreamM := &captureUpstreamMetrics{m: metrics}
	entries := map[engine.Name]engine.RegistryEntry{}
	for name, ec := range cfg.Engines {
		if !ec.Enable {
			continue
		}
		c, err := httpclient.New(ec)
		require.NoError(t, err)
		// Wire the same auth + outbound observability the production
		// composition root does, so integration tests exercise the
		// real metric counters AND the real outbound debug log.
		c.WithAuthMetrics(authM).
			Instrument(httpclient.InstrumentOptions{
				Logger:       logger,
				Recorder:     upstreamM,
				EngineName:   name,
				AuthHeader:   ec.AuthHeader,
				AuthScheme:   string(ec.AuthScheme),
				BodyLogBytes: ec.ResolveUpstream(cfg.Observability.Upstream).BodyLogBytes,
			})
		br := breaker.New(name, ec.Breaker, nil)
		ad, err := buildAdapter(engine.Name(name), ec, c, br)
		require.NoError(t, err)
		entries[engine.Name(name)] = engine.RegistryEntry{Engine: ad, MimeTypes: ec.MimeTypes}
	}
	registry, err := engine.NewRegistry(entries, engine.Name(cfg.Routing.DefaultEngine))
	require.NoError(t, err)

	handler, err := httptransport.NewRouter(httptransport.RouterDeps{
		Config:   cfg,
		Logger:   logger,
		Metrics:  metrics,
		Registry: registry,
	})
	require.NoError(t, err)

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv, cap
}

// captureAuthMetrics + captureUpstreamMetrics are the production
// metric adapters in miniature — they exist in app.go but are
// unexported, so we duplicate them here. A future refactor that
// exports them lets these stubs vanish.
type captureAuthMetrics struct{ m *observability.Metrics }

func (a *captureAuthMetrics) RecordAuthAttempt(eng, scheme string, out authutil.Outcome) {
	if a.m == nil || a.m.EngineAuthAttached == nil {
		return
	}
	a.m.EngineAuthAttached.WithLabelValues(eng, scheme, string(out)).Inc()
}

type captureUpstreamMetrics struct{ m *observability.Metrics }

func (u *captureUpstreamMetrics) RecordUpstreamStatus(eng string, status int) {
	if u.m == nil || u.m.EngineUpstreamStatus == nil {
		return
	}
	u.m.EngineUpstreamStatus.WithLabelValues(eng, fmt.Sprintf("%d", status)).Inc()
}

// buildAdapter mirrors internal/app/app.go::newAdapterByCompat (kept
// out of band so the integration tests can wire engines without
// exporting that function).
func buildAdapter(name engine.Name, ec config.EngineConfig, c *httpclient.Client, br *breaker.Breaker) (engine.Engine, error) {
	switch ec.CompatType {
	case config.CompatDocling:
		return docling.New(name, ec, c, br)
	case config.CompatExternal:
		return external.New(name, ec, c, br)
	case config.CompatTika:
		return tika.New(name, ec, c, br)
	case config.CompatDoclingExternal:
		return doclingexternal.New(name, ec, c, br)
	default:
		return nil, fmt.Errorf("unknown compat_type %q", ec.CompatType)
	}
}

func waitForReadiness(t *testing.T, url string, timeout time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			t.Fatalf("readiness timeout: %s", url)
		default:
		}
		resp, err := newClient().Get(url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode < 500 {
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("readiness timeout: %s", url)
}
