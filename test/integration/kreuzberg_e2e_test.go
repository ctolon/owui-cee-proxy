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
	"github.com/ctolon/owui-cee-proxy/internal/engine"
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

// TestKreuzberg_MsgRouting is the end-to-end regression test for the
// `.msg` HTTP 400 bug. Before the extension-routing change, multipart
// uploads with CFB-format .msg bodies routed to PowerPoint-claiming
// engines (because mimetype.Detect collapses every CFB container to
// application/vnd.ms-powerpoint), which backends rejected with 400.
//
// The test pins down the fix by:
//
//  1. Declaring kreuzberg with `extensions: [".msg"]` and the default
//     strategy (mime_then_extension).
//  2. Uploading the sample.msg fixture.
//  3. Verifying kreuzberg returns 200 (real backend, real CFB body).
//  4. Inspecting the access-log to confirm BOTH the MIME-resolution
//     phase (mime_source=sniffed_then_extension or extension) AND the
//     dispatch phase (pick_source=extension) chose the right path.
func TestKreuzberg_MsgRouting(t *testing.T) {
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
	cfg.Routing.Strategy = engine.StrategyMimeThenExt
	cfg.Engines = map[string]config.EngineConfig{
		"kreuzberg": {
			Enable:         true,
			CompatType:     config.CompatDocling,
			URL:            kbURL,
			AuthHeader:     "Authorization",
			AuthScheme:     "bearer",
			Extensions:     []string{".msg"},
			ForwardOptions: map[string]string{"output_format": "markdown"},
		},
	}

	proxy, logCap := newProxyServerWithLogCapture(t, cfg)
	waitForReadiness(t, proxy.URL+"/healthz", 30*time.Second)

	body, contentType := buildMultipart(t, fixturePath(t, "sample.msg"), "files")
	resp, err := newClient().Post(proxy.URL+"/v1/convert/file", contentType, body)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode,
		"CFB-disambiguation MUST route .msg uploads to kreuzberg and earn a 200")

	var env struct {
		Status   string `json:"status"`
		Document struct {
			MdContent string `json:"md_content"`
		} `json:"document"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&env))
	require.Equal(t, "success", env.Status)

	// The access log MUST surface the routing decision so operators can
	// audit "why did this .msg upload land on kreuzberg?" from one
	// record without joining streams. Either source value is acceptable:
	// sniffed_then_extension when mimetype.Detect produced a CFB hit,
	// extension when the sniff fell short and the filename was the
	// sole signal — both prove the new pipeline kicked in.
	entry := logCap.findEventForPath("request_completed", "/v1/convert/file")
	require.NotNil(t, entry, "request_completed access-log entry missing")
	require.Equal(t, "kreuzberg", entry["engine"])
	require.Equal(t, "extension", entry["pick_source"])
	require.Equal(t, "mime_then_extension", entry["routing_strategy"])
	require.Contains(t, []any{"sniffed_then_extension", "extension"}, entry["mime_source"],
		"mime_source must record the new CFB-disambiguation path")
}
