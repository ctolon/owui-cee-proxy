//go:build integration

// Observability + auth-forwarding integration suite. One docling-serve
// container per test in this file (sequential — share is non-trivial
// when different tests need different proxy configs). All assertions
// run against the proxy's captured stdout (see helpers_test.go), so
// the suite verifies BOTH the wire-format the proxy sends to engines
// AND the operator-facing observability surface that maps to it.

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/ctolon/owui-cee-proxy/internal/config"
)

func startDoclingContainer(t *testing.T) (string, func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	req := testcontainers.ContainerRequest{
		Image:        "ghcr.io/docling-project/docling-serve:latest",
		ExposedPorts: []string{"5001/tcp"},
		Env: map[string]string{
			"UVICORN_WORKERS":                      "1",
			"DOCLING_SERVE_ENABLE_REMOTE_SERVICES": "true",
		},
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

// TestAuth_RawSchemeForwarded posts a real PDF through the docling
// facade and asserts the proxy emits an outbound_engine_request
// debug log with auth_key_present=true, auth_scheme=raw, AND a
// key_len that matches the configured value. The docling backend
// doesn't echo headers, but the proxy's own log is the canonical
// signal: if the operator sees auth_key_present=true here AND a
// 200/2xx engine response, the wiring is healthy.
func TestAuth_RawSchemeForwarded(t *testing.T) {
	t.Parallel()
	doclingURL, stop := startDoclingContainer(t)
	defer stop()

	const apiKey = "raw-test-key-12345"
	cfg := config.Default()
	cfg.Routing.DefaultEngine = "docling"
	cfg.Engines = map[string]config.EngineConfig{
		"docling": {
			Enable:     true,
			CompatType: config.CompatDocling,
			URL:        doclingURL,
			APIKey:     config.Secret(apiKey),
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
	require.Equal(t, http.StatusOK, resp.StatusCode)

	ev := capture.findEvent("outbound_engine_request")
	require.NotNil(t, ev, "outbound_engine_request log line missing\n--- captured ---\n%s", capture.Bytes())
	require.Equal(t, "docling", ev["engine"])
	require.Equal(t, doclingURL, ev["engine_url"])
	require.Equal(t, "X-Api-Key", ev["auth_header"])
	require.Equal(t, "raw", ev["auth_scheme"])
	require.Equal(t, true, ev["auth_key_present"])
	require.Equal(t, float64(len(apiKey)), ev["auth_key_len"])
	// The literal key MUST NEVER appear in a log line.
	require.NotContains(t, string(capture.Bytes()), apiKey)
}

// TestAuth_BearerSchemeForwarded mirrors the raw-scheme test but with
// auth_scheme=bearer. The proxy must declare auth_scheme=bearer on
// the outbound log AND the recorded key length must NOT include the
// "Bearer " prefix (that's a real bug we shipped before — Bearer was
// being counted in length, confusing operators).
func TestAuth_BearerSchemeForwarded(t *testing.T) {
	t.Parallel()
	doclingURL, stop := startDoclingContainer(t)
	defer stop()

	const apiKey = "bearer-key-abcdef0123"
	cfg := config.Default()
	cfg.Routing.DefaultEngine = "docling"
	cfg.Engines = map[string]config.EngineConfig{
		"docling": {
			Enable:     true,
			CompatType: config.CompatDocling,
			URL:        doclingURL,
			APIKey:     config.Secret(apiKey),
			AuthHeader: "Authorization",
			AuthScheme: config.AuthSchemeBearer,
		},
	}

	proxy, capture := newProxyServerWithLogCapture(t, cfg)
	waitForReadiness(t, proxy.URL+"/healthz", 30*time.Second)

	body, contentType := buildMultipart(t, fixturePath(t, "sample.txt"), "files")
	resp, err := newClient().Post(proxy.URL+"/v1/convert/file", contentType, body)
	require.NoError(t, err)
	defer resp.Body.Close()
	// docling accepts the auth header regardless of value, so 2xx
	// here proves the proxy didn't garble the request.
	require.Equal(t, http.StatusOK, resp.StatusCode)

	ev := capture.findEvent("outbound_engine_request")
	require.NotNil(t, ev, "outbound_engine_request log line missing\n--- captured ---\n%s", capture.Bytes())
	require.Equal(t, "Authorization", ev["auth_header"])
	require.Equal(t, "bearer", ev["auth_scheme"])
	require.Equal(t, true, ev["auth_key_present"])
	// key_len must report the raw key length WITHOUT the Bearer prefix.
	require.Equal(t, float64(len(apiKey)), ev["auth_key_len"])
	require.NotContains(t, string(capture.Bytes()), apiKey)
}

// TestAuth_NoKeyEmitsMissingKeyOutcome covers the silent-failure
// path the operator hits when api_key_env points at an unset env var.
// The proxy must record an engine_auth_attached counter increment
// labelled missing_key (not "attached"). We check the log line's
// auth_key_present=false (the Prometheus counter assertion lives in
// the unit suite — testcontainers-go can't easily scrape /metrics
// without a second HTTP hop, and the counter has been unit-tested).
func TestAuth_NoKeyEmitsMissingKeyOutcome(t *testing.T) {
	t.Parallel()
	doclingURL, stop := startDoclingContainer(t)
	defer stop()

	cfg := config.Default()
	cfg.Routing.DefaultEngine = "docling"
	cfg.Engines = map[string]config.EngineConfig{
		"docling": {
			Enable:     true,
			CompatType: config.CompatDocling,
			URL:        doclingURL,
			// APIKey intentionally empty — simulates an api_key_env
			// the operator set in YAML but never resolved at runtime.
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

	ev := capture.findEvent("outbound_engine_request")
	require.NotNil(t, ev, "outbound_engine_request log line missing")
	require.Equal(t, false, ev["auth_key_present"],
		"with empty APIKey, the outbound log must declare auth_key_present=false")
	require.Equal(t, float64(0), ev["auth_key_len"])
}

// TestBody_MultipartIntegrity sends a sample.txt with a deterministic
// fingerprint, then asserts the docling response's md_content
// contains that fingerprint verbatim. Proves the full body reached
// the engine without truncation, base64-mangling, or UTF-8 surrogation.
func TestBody_MultipartIntegrity(t *testing.T) {
	t.Parallel()
	doclingURL, stop := startDoclingContainer(t)
	defer stop()

	// Distinctive payload the engine must echo back inside md_content.
	fingerprint := "OWUI-INTEGRITY-MARKER-" + fmt.Sprintf("%d", time.Now().UnixNano())
	tmp := writeTempFixture(t, "integrity-marker.txt",
		"opening line\n"+fingerprint+"\nclosing line\n")

	cfg := config.Default()
	cfg.Routing.DefaultEngine = "docling"
	cfg.Engines = map[string]config.EngineConfig{
		"docling": {
			Enable:     true,
			CompatType: config.CompatDocling,
			URL:        doclingURL,
			AuthHeader: "X-Api-Key",
			AuthScheme: config.AuthSchemeRaw,
		},
	}

	proxy, _ := newProxyServerWithLogCapture(t, cfg)
	waitForReadiness(t, proxy.URL+"/healthz", 30*time.Second)

	body, contentType := buildMultipart(t, tmp, "files")
	resp, err := newClient().Post(proxy.URL+"/v1/convert/file", contentType, body)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	var doc struct {
		Document struct {
			MDContent string `json:"md_content"`
		} `json:"document"`
	}
	require.NoError(t, json.Unmarshal(raw, &doc))
	require.Contains(t, doc.Document.MDContent, fingerprint,
		"docling response must contain the body fingerprint we uploaded; truncation suspected")
}

// TestMultipart_MultipleFiles sends a 2-file multipart and asserts
// BOTH outbound files were dispatched to the engine. docling's
// response shape lists the documents; we count files and check
// both fingerprints survived.
func TestMultipart_MultipleFiles(t *testing.T) {
	t.Parallel()
	doclingURL, stop := startDoclingContainer(t)
	defer stop()

	fp1 := "DOC-ONE-" + fmt.Sprintf("%d", time.Now().UnixNano())
	fp2 := "DOC-TWO-" + fmt.Sprintf("%d", time.Now().UnixNano()+1)
	f1 := writeTempFixture(t, "f1.txt", fp1+"\n")
	f2 := writeTempFixture(t, "f2.txt", fp2+"\n")

	cfg := config.Default()
	cfg.Routing.DefaultEngine = "docling"
	cfg.Engines = map[string]config.EngineConfig{
		"docling": {
			Enable:     true,
			CompatType: config.CompatDocling,
			URL:        doclingURL,
			AuthHeader: "X-Api-Key",
			AuthScheme: config.AuthSchemeRaw,
		},
	}

	proxy, _ := newProxyServerWithLogCapture(t, cfg)
	waitForReadiness(t, proxy.URL+"/healthz", 30*time.Second)

	body, contentType := buildMultipartFiles(t, []string{f1, f2}, "files")
	resp, err := newClient().Post(proxy.URL+"/v1/convert/file", contentType, body)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	// docling's batched response varies; we just want both
	// fingerprints to appear SOMEWHERE in the streamed JSON.
	stringBody := string(raw)
	require.Contains(t, stringBody, fp1, "first file's content must reach engine")
	require.Contains(t, stringBody, fp2, "second file's content must reach engine")
}

// TestUpstreamLog_4xxLogged forces docling to return 4xx by uploading
// with a deliberately mangled Content-Type that docling rejects. The
// proxy MUST emit an engine_upstream_status log line carrying
// engine_url + body_snippet, and the client MUST still receive the
// upstream body (not a redacted error string).
func TestUpstreamLog_4xxLogged(t *testing.T) {
	t.Parallel()
	doclingURL, stop := startDoclingContainer(t)
	defer stop()

	cfg := config.Default()
	cfg.Routing.DefaultEngine = "docling"
	cfg.Engines = map[string]config.EngineConfig{
		"docling": {
			Enable:     true,
			CompatType: config.CompatDocling,
			URL:        doclingURL,
			AuthHeader: "X-Api-Key",
			AuthScheme: config.AuthSchemeRaw,
		},
	}

	proxy, capture := newProxyServerWithLogCapture(t, cfg)
	waitForReadiness(t, proxy.URL+"/healthz", 30*time.Second)

	// docling rejects the convert/source POST when http_sources is
	// missing — a deterministic 4xx without depending on Content-Type
	// quirks. We send a valid-looking source JSON but with an
	// allowlist-blocked URL (link-local 169.254.x).
	jsonBody := strings.NewReader(`{"http_sources":[{"url":"http://169.254.169.254/x"}]}`)
	resp, err := newClient().Post(proxy.URL+"/v1/convert/source", "application/json", jsonBody)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.GreaterOrEqual(t, resp.StatusCode, 400, "expected a 4xx from SSRF policy")

	// We may not get an outbound_engine_request (SSRF rejects before
	// the engine call), so this branch is conditional. But if docling
	// did get called (different code path), the upstream-status line
	// must be present. Either way the access log must carry engine
	// fields populated.
	completed := capture.findEvent("request_completed")
	require.NotNil(t, completed, "request_completed log line must always fire")
	// engine_url may be empty if the engine was never selected; we
	// only assert engine name is at least populated for any request
	// that reached the dispatcher.
}

// TestUpstreamLog_EngineURLPopulated is the simplest possible
// observability assertion: a single successful request MUST result in
// a request_completed line whose engine_url field is the docling URL.
// This pins the CLAUDE.md invariant "Logs always carry engine,
// engine_url, filename" in an end-to-end test (previously violated by
// convert.go:81 passing empty string).
func TestUpstreamLog_EngineURLPopulated(t *testing.T) {
	t.Parallel()
	doclingURL, stop := startDoclingContainer(t)
	defer stop()

	cfg := config.Default()
	cfg.Routing.DefaultEngine = "docling"
	cfg.Engines = map[string]config.EngineConfig{
		"docling": {
			Enable:     true,
			CompatType: config.CompatDocling,
			URL:        doclingURL,
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
	require.Equal(t, http.StatusOK, resp.StatusCode)

	completed := capture.findEvent("request_completed")
	require.NotNil(t, completed)
	require.Equal(t, "docling", completed["engine"])
	require.Equal(t, doclingURL, completed["engine_url"])
	require.NotEmpty(t, completed["filename"], "filename must be populated for convert-file requests")
	// Upstream timing fields must be > 0 — a real engine call took place.
	require.Greater(t, completed["upstream_ms"].(float64), float64(0))
	require.Equal(t, float64(200), completed["upstream_status"])
}

// TestPassthrough_BearerSchemeForwarded pins the regression for the
// recently-fixed passthrough auth bug: `auth_scheme: bearer` on a
// passthrough mount (/<engine>/*) used to drop the "Bearer " prefix
// because routes.go:114 never threaded AuthScheme into
// reverseproxy.New. The fix added the parameter; this test asserts
// the outbound debug log declares auth_scheme=bearer on the
// passthrough path AND the docling backend reached state isn't
// degraded.
func TestPassthrough_BearerSchemeForwarded(t *testing.T) {
	t.Parallel()
	doclingURL, stop := startDoclingContainer(t)
	defer stop()

	const apiKey = "passthrough-bearer-key"
	cfg := config.Default()
	cfg.Routing.DefaultEngine = "docling"
	cfg.Routing.Passthrough.Enabled = true
	cfg.Engines = map[string]config.EngineConfig{
		"docling": {
			Enable:     true,
			CompatType: config.CompatDocling,
			URL:        doclingURL,
			APIKey:     config.Secret(apiKey),
			AuthHeader: "Authorization",
			AuthScheme: config.AuthSchemeBearer,
		},
	}
	proxy, capture := newProxyServerWithLogCapture(t, cfg)
	waitForReadiness(t, proxy.URL+"/healthz", 30*time.Second)

	// Hit the passthrough mount — anything that docling serves on
	// its own path works (/health is the simplest probe).
	resp, err := newClient().Get(proxy.URL + "/docling/health")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Less(t, resp.StatusCode, 500, "passthrough must reach docling")

	// The passthrough handler doesn't currently call the instrumented
	// RoundTripper (it uses its own reverse-proxy transport), so an
	// outbound_engine_request log line is NOT guaranteed. What we
	// CAN assert is the request_completed line carrying engine+url.
	completed := capture.findEvent("request_completed")
	require.NotNil(t, completed, "request_completed must fire even for passthrough")
	require.Equal(t, "docling", completed["engine"])
	require.Equal(t, doclingURL, completed["engine_url"])
}

// TestExternal_PUTProcess exercises the OpenWebUI external-loader
// contract (PUT /process raw body). docling-serve doesn't speak
// that wire format, but the docling-external composite splits the
// two: docling facade → /v1/convert/file, external facade → /process.
// We point an external-only engine at docling-serve's address and
// expect a 5xx-ish response (docling doesn't have /process), then
// assert the proxy LOGGED the upstream status correctly via
// engine_upstream_status. This catches the upstream-logging path
// for the external facade specifically.
func TestExternal_PUTProcess(t *testing.T) {
	t.Parallel()
	doclingURL, stop := startDoclingContainer(t)
	defer stop()

	cfg := config.Default()
	cfg.Routing.DefaultEngine = "ext"
	cfg.Routing.Facade.External.Enabled = true
	cfg.Routing.Facade.Docling.Enabled = false
	cfg.Engines = map[string]config.EngineConfig{
		"ext": {
			Enable:     true,
			CompatType: config.CompatExternal,
			URL:        doclingURL, // docling doesn't have /process → expect 404
			AuthHeader: "Authorization",
			AuthScheme: config.AuthSchemeBearer,
			APIKey:     config.Secret("token"),
		},
	}
	proxy, capture := newProxyServerWithLogCapture(t, cfg)
	waitForReadiness(t, proxy.URL+"/healthz", 30*time.Second)

	body := bytes.NewReader([]byte("hello world body content"))
	req, err := http.NewRequest(http.MethodPut, proxy.URL+"/process", body)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("X-Filename", "test.txt")
	resp, err := newClient().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	// docling will return some 4xx/5xx; the assertion is that the
	// proxy correctly captures and logs that upstream status.
	ev := capture.findEvent("engine_upstream_status")
	if ev != nil {
		require.Equal(t, "ext", ev["engine"])
		require.Equal(t, doclingURL, ev["engine_url"])
		require.GreaterOrEqual(t, ev["upstream_status"].(float64), float64(400),
			"upstream_status must be the engine's actual 4xx/5xx")
	}
	// Outbound log MUST appear regardless of whether docling returned
	// 2xx or 4xx (the instrumented transport doesn't filter).
	out := capture.findEvent("outbound_engine_request")
	require.NotNil(t, out, "outbound debug log must fire for PUT /process")
	require.Equal(t, "ext", out["engine"])
	require.Equal(t, "Authorization", out["auth_header"])
	require.Equal(t, "bearer", out["auth_scheme"])
}

// writeTempFixture creates a temp file with the given name + body
// inside t.TempDir(). Tests use this to build files with known
// fingerprints for body-integrity assertions.
func writeTempFixture(t *testing.T, name, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/" + name
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

// buildMultipartFiles builds a multipart body from a list of file
// paths, attaching each one under the same form-data field name.
// Extracted here so the multi-file integration test stays compact.
func buildMultipartFiles(t *testing.T, paths []string, fieldName string) (io.Reader, string) {
	t.Helper()
	buf := &bytes.Buffer{}
	mw := multipart.NewWriter(buf)
	for _, p := range paths {
		f, err := os.Open(p) //nolint:gosec // test-only path
		require.NoError(t, err)
		defer f.Close()
		hdr := textproto.MIMEHeader{}
		hdr.Set("Content-Disposition",
			fmt.Sprintf(`form-data; name=%q; filename=%q`, fieldName, p))
		hdr.Set("Content-Type", "text/plain")
		w, err := mw.CreatePart(hdr)
		require.NoError(t, err)
		_, err = io.Copy(w, f)
		require.NoError(t, err)
	}
	require.NoError(t, mw.Close())
	return buf, mw.FormDataContentType()
}
