//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/ctolon/owui-cee-proxy/internal/config"
)

func TestDocling_FacadeIsTransparent(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	req := testcontainers.ContainerRequest{
		Image:        "ghcr.io/docling-project/docling-serve:latest",
		ExposedPorts: []string{"5001/tcp"},
		Env: map[string]string{
			"UVICORN_WORKERS":                       "1",
			"DOCLING_SERVE_ENABLE_REMOTE_SERVICES":  "true",
		},
		WaitingFor: wait.ForHTTP("/health").WithPort("5001/tcp").WithStartupTimeout(8 * time.Minute),
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	require.NoError(t, err)
	defer func() { _ = c.Terminate(ctx) }()
	host, _ := c.Host(ctx)
	port, _ := c.MappedPort(ctx, "5001")
	doclingURL := "http://" + host + ":" + port.Port()

	cfg := config.Default()
	cfg.Routing.DefaultCEEEngine = "docling"
	cfg.Engines.Docling.Enable = true
	cfg.Engines.Docling.URL = doclingURL

	proxy := newProxyServer(t, cfg)
	waitForReadiness(t, proxy.URL+"/healthz", 30*time.Second)

	body, contentType := buildMultipart(t, fixturePath(t, "sample.txt"), "files")
	resp, err := newClient().Post(proxy.URL+"/v1/convert/file", contentType, body)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var raw map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&raw))
	// Docling returns "document" with "md_content"; ensure we got non-empty content.
	doc, ok := raw["document"].(map[string]any)
	require.True(t, ok)
	require.NotEmpty(t, doc["md_content"])
}
