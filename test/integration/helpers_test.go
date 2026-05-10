//go:build integration

package integration

import (
	"context"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ctolon/owui-cee-proxy/internal/breaker"
	"github.com/ctolon/owui-cee-proxy/internal/config"
	"github.com/ctolon/owui-cee-proxy/internal/engine"
	"github.com/ctolon/owui-cee-proxy/internal/engine/compat/docling"
	"github.com/ctolon/owui-cee-proxy/internal/engine/compat/doclingexternal"
	"github.com/ctolon/owui-cee-proxy/internal/engine/compat/external"
	"github.com/ctolon/owui-cee-proxy/internal/engine/compat/tika"
	"github.com/ctolon/owui-cee-proxy/internal/httpclient"
	"github.com/ctolon/owui-cee-proxy/internal/observability"
	httptransport "github.com/ctolon/owui-cee-proxy/internal/transport/http"
)

func newProxyServer(t *testing.T, cfg *config.Config) *httptest.Server {
	t.Helper()

	logger := observability.NewLogger(cfg.Observability.Log)
	metrics := observability.NewMetrics()

	entries := map[engine.Name]engine.RegistryEntry{}
	for name, ec := range cfg.Engines {
		if !ec.Enable {
			continue
		}
		c, err := httpclient.New(ec)
		require.NoError(t, err)
		br := breaker.New(name, ec.Breaker)
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
	return srv
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
