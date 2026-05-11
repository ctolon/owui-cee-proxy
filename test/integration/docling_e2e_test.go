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

// doclingAPIKey is the shared secret the test boots the docling-serve
// container with. The container then enforces "X-Api-Key: <this>" on
// every request — a request without the header or with a different
// value comes back 401. The proxy is the one that has to attach it
// on outbound calls, which is exactly what this suite verifies end-
// to-end.
const doclingAPIKey = "owui-cee-proxy-e2e-secret-3kj4h2k4j"

// startDoclingWithAPIKey boots a docling-serve container that requires
// X-Api-Key=doclingAPIKey on every request. Returns the URL + a
// teardown closure. Kept here (instead of helpers_test.go) because
// the API-key flavour is specific to the auth-forwarding tests below
// — every other test in the suite already has its own zero-config
// docling helper.
func startDoclingWithAPIKey(t *testing.T) (string, func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	req := testcontainers.ContainerRequest{
		Image:        "ghcr.io/docling-project/docling-serve:latest",
		ExposedPorts: []string{"5001/tcp"},
		Env: map[string]string{
			"UVICORN_WORKERS":                      "1",
			"DOCLING_SERVE_ENABLE_REMOTE_SERVICES": "true",
			// docling-serve enforces the API key on every inbound
			// request when this env var is set. The expected wire
			// format is the X-Api-Key header (NOT Authorization
			// Bearer); requests without it / with a wrong value get
			// HTTP 401.
			"DOCLING_SERVE_API_KEY": doclingAPIKey,
		},
		// /health is always public — no API key needed on the probe.
		WaitingFor: wait.ForHTTP("/health").WithPort("5001/tcp").WithStartupTimeout(8 * time.Minute),
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	require.NoError(t, err)
	host, _ := c.Host(ctx)
	port, _ := c.MappedPort(ctx, "5001")
	url := "http://" + host + ":" + port.Port()
	return url, func() { _ = c.Terminate(context.Background()) }
}

// TestDocling_FacadeIsTransparent now BOOTS docling with a required
// API key and configures the proxy with the matching key. The fact
// that the call succeeds (200 + non-empty md_content) is the proof
// that the proxy stamps X-Api-Key on the outbound multipart request
// — a regression that drops the header would surface as 401 here
// before the response body assertion fires.
func TestDocling_FacadeIsTransparent(t *testing.T) {
	t.Parallel()
	doclingURL, stop := startDoclingWithAPIKey(t)
	defer stop()

	cfg := config.Default()
	cfg.Routing.DefaultEngine = "docling"
	cfg.Engines = map[string]config.EngineConfig{
		"docling": {
			Enable:     true,
			CompatType: config.CompatDocling,
			URL:        doclingURL,
			APIKey:     config.Secret(doclingAPIKey),
			AuthHeader: "X-Api-Key",
			AuthScheme: config.AuthSchemeRaw,
		},
	}

	proxy := newProxyServer(t, cfg)
	waitForReadiness(t, proxy.URL+"/healthz", 30*time.Second)

	body, contentType := buildMultipart(t, fixturePath(t, "sample.txt"), "files")
	resp, err := newClient().Post(proxy.URL+"/v1/convert/file", contentType, body)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode,
		"docling rejected the proxy-stamped X-Api-Key — auth forwarding is broken")

	var raw map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&raw))
	// Docling returns "document" with "md_content"; ensure we got non-empty content.
	doc, ok := raw["document"].(map[string]any)
	require.True(t, ok)
	require.NotEmpty(t, doc["md_content"])
}

// TestDocling_WrongAPIKeyIsRejected is the negative-test sibling: the
// proxy is configured with a key that does NOT match what docling
// expects, so docling MUST 401. The interesting invariant here is
// that the proxy propagates the 401 transparently to the client —
// it doesn't masquerade upstream auth failures as a 5xx or strip
// the response body.
func TestDocling_WrongAPIKeyIsRejected(t *testing.T) {
	t.Parallel()
	doclingURL, stop := startDoclingWithAPIKey(t)
	defer stop()

	cfg := config.Default()
	cfg.Routing.DefaultEngine = "docling"
	cfg.Engines = map[string]config.EngineConfig{
		"docling": {
			Enable:     true,
			CompatType: config.CompatDocling,
			URL:        doclingURL,
			APIKey:     config.Secret("WRONG-KEY-deliberately-mismatched"),
			AuthHeader: "X-Api-Key",
			AuthScheme: config.AuthSchemeRaw,
		},
	}

	proxy := newProxyServer(t, cfg)
	waitForReadiness(t, proxy.URL+"/healthz", 30*time.Second)

	body, contentType := buildMultipart(t, fixturePath(t, "sample.txt"), "files")
	resp, err := newClient().Post(proxy.URL+"/v1/convert/file", contentType, body)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode,
		"docling 401 must be propagated transparently, not masked as a 5xx")
}

// TestDocling_MissingAPIKeyIsRejected drops the proxy-side key
// entirely (APIKey: ""). authutil.Apply then takes the "missing_key"
// branch — auth header is NOT stamped — and docling 401s the
// outbound request. End-to-end proof that the proxy doesn't
// fabricate an empty-value header (some backends 401 on
// "X-Api-Key:" empty just as readily as on a wrong value, and the
// proxy's previous bug class was precisely the "send the header
// anyway with empty value" failure mode).
func TestDocling_MissingAPIKeyIsRejected(t *testing.T) {
	t.Parallel()
	doclingURL, stop := startDoclingWithAPIKey(t)
	defer stop()

	cfg := config.Default()
	cfg.Routing.DefaultEngine = "docling"
	cfg.Engines = map[string]config.EngineConfig{
		"docling": {
			Enable:     true,
			CompatType: config.CompatDocling,
			URL:        doclingURL,
			// APIKey intentionally empty — simulates an unresolved
			// api_key_env (the user's real-world "I set the env var
			// but it didn't reach the Pod" failure).
			APIKey:     config.Secret(""),
			AuthHeader: "X-Api-Key",
			AuthScheme: config.AuthSchemeRaw,
		},
	}

	proxy, capture := newProxyServerWithLogCapture(t, cfg)
	waitForReadiness(t, proxy.URL+"/healthz", 30*time.Second)

	body, contentType := buildMultipart(t, fixturePath(t, "sample.txt"), "files")
	resp, err := newClient().Post(proxy.URL+"/v1/convert/file", contentType, body)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode,
		"docling 401 must be propagated transparently when the proxy has no key to attach")

	// The instrumented transport must record the missing-key path
	// (auth_key_present=false) — proves the proxy decided NOT to
	// stamp the header rather than stamping it with an empty value.
	out := capture.findEventForPath("outbound_engine_request", "/v1/convert/file")
	require.NotNil(t, out, "outbound_engine_request log line missing")
	require.Equal(t, false, out["auth_key_present"],
		"with empty APIKey the proxy MUST NOT stamp the auth header")
	require.Equal(t, float64(0), out["auth_key_len"])
}
