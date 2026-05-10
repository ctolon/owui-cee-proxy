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

func TestKreuzberg_FacadeReturnsDoclingShape(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	req := testcontainers.ContainerRequest{
		Image:        "ghcr.io/kreuzberg-dev/kreuzberg:latest",
		ExposedPorts: []string{"8000/tcp"},
		WaitingFor:   wait.ForHTTP("/health").WithPort("8000/tcp").WithStartupTimeout(3 * time.Minute),
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	require.NoError(t, err)
	defer func() { _ = c.Terminate(ctx) }()
	host, _ := c.Host(ctx)
	port, _ := c.MappedPort(ctx, "8000")
	kbURL := "http://" + host + ":" + port.Port()

	cfg := config.Default()
	cfg.Routing.DefaultEngine = "kreuzberg"
	cfg.Engines = map[string]config.EngineConfig{
		"kreuzberg": {
			Enable:         true,
			CompatType:     config.CompatDocling, // Kreuzberg's /v1/convert/file is docling-compat.
			URL:            kbURL,
			AuthHeader:     "Authorization",
			AuthScheme:     "bearer",
			ForwardOptions: map[string]string{"output_format": "markdown"},
		},
	}

	proxy := newProxyServer(t, cfg)
	waitForReadiness(t, proxy.URL+"/healthz", 30*time.Second)

	body, contentType := buildMultipart(t, fixturePath(t, "sample.txt"), "files")
	resp, err := newClient().Post(proxy.URL+"/v1/convert/file", contentType, body)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var env struct {
		Status   string `json:"status"`
		Document struct {
			MdContent string `json:"md_content"`
		} `json:"document"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&env))
	require.Equal(t, "success", env.Status)
	require.NotEmpty(t, env.Document.MdContent)
}
