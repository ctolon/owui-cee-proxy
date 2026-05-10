//go:build integration

package integration

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ctolon/owui-cee-proxy/internal/breaker"
	"github.com/ctolon/owui-cee-proxy/internal/config"
	"github.com/ctolon/owui-cee-proxy/internal/engine"
	"github.com/ctolon/owui-cee-proxy/internal/engine/docling"
	"github.com/ctolon/owui-cee-proxy/internal/engine/kreuzberg"
	"github.com/ctolon/owui-cee-proxy/internal/engine/tika"
	"github.com/ctolon/owui-cee-proxy/internal/httpclient"
	"github.com/ctolon/owui-cee-proxy/internal/observability"
	httptransport "github.com/ctolon/owui-cee-proxy/internal/transport/http"
)

func newProxyServer(t *testing.T, cfg *config.Config) *httptest.Server {
	t.Helper()

	logger := observability.NewLogger(cfg.Observability.Log)
	metrics := observability.NewMetrics()

	entries := map[engine.Name]engine.RegistryEntry{}
	if cfg.Engines.Docling.Enable {
		c, err := httpclient.New(cfg.Engines.Docling)
		require.NoError(t, err)
		ad, err := docling.New(cfg.Engines.Docling, c, breaker.New("docling", cfg.Engines.Docling.Breaker))
		require.NoError(t, err)
		entries[engine.Docling] = engine.RegistryEntry{Engine: ad, MimeTypes: cfg.Engines.Docling.MimeTypes}
	}
	if cfg.Engines.Tika.Enable {
		c, err := httpclient.New(cfg.Engines.Tika)
		require.NoError(t, err)
		ad, err := tika.New(cfg.Engines.Tika, c, breaker.New("tika", cfg.Engines.Tika.Breaker))
		require.NoError(t, err)
		entries[engine.Tika] = engine.RegistryEntry{Engine: ad, MimeTypes: cfg.Engines.Tika.MimeTypes}
	}
	if cfg.Engines.Kreuzberg.Enable {
		c, err := httpclient.New(cfg.Engines.Kreuzberg)
		require.NoError(t, err)
		ad, err := kreuzberg.New(cfg.Engines.Kreuzberg, c, breaker.New("kreuzberg", cfg.Engines.Kreuzberg.Breaker))
		require.NoError(t, err)
		entries[engine.Kreuzberg] = engine.RegistryEntry{Engine: ad, MimeTypes: cfg.Engines.Kreuzberg.MimeTypes}
	}
	registry, err := engine.NewRegistry(entries, engine.Name(cfg.Routing.DefaultCEEEngine))
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
