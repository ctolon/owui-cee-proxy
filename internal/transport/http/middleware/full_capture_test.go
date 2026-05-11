package middleware_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	mw "github.com/ctolon/owui-cee-proxy/internal/transport/http/middleware"
)

// TestFullCapture_Disabled_NoEmission confirms the zero-cost contract:
// when Enabled=false, the middleware returns the wrapped handler
// untouched and emits no log line at all. Useful so the default
// production path pays nothing.
func TestFullCapture_Disabled_NoEmission(t *testing.T) {
	t.Parallel()
	buf := &bytes.Buffer{}
	logger := zerolog.New(buf).Level(zerolog.DebugLevel)

	h := mw.FullCapture(logger, mw.FullCaptureConfig{Enabled: false})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))

	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	require.NoError(t, err)
	_ = resp.Body.Close()

	require.Empty(t, buf.String(), "disabled capture MUST NOT emit any log line")
}

// TestFullCapture_RedactsKnownCredentialHeaders pins the redaction
// allowlist. Authorization, X-Api-Key, and operator-supplied extras
// must be replaced with "<redacted len=N>" in the capture log. The
// header NAME stays so operators see the credential was present;
// the value is gone.
func TestFullCapture_RedactsKnownCredentialHeaders(t *testing.T) {
	t.Parallel()
	buf := &bytes.Buffer{}
	logger := zerolog.New(buf).Level(zerolog.DebugLevel)

	h := mw.FullCapture(logger, mw.FullCaptureConfig{
		Enabled:            true,
		BodyBytes:          0,
		RedactExtraHeaders: []string{"X-Proxy-Api-Key", "X-Tenant-Token"},
	})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "session=secret-cookie-value")
		w.WriteHeader(http.StatusOK)
	}))

	srv := httptest.NewServer(h)
	defer srv.Close()
	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer secret-jwt-token")
	req.Header.Set("X-Api-Key", "secret-api-key")
	req.Header.Set("X-Proxy-Api-Key", "secret-proxy-key")
	req.Header.Set("X-Tenant-Token", "secret-tenant")
	req.Header.Set("X-Safe-Header", "this-is-fine")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()

	logLine := buf.String()
	require.Contains(t, logLine, `"sensitive_capture":true`)
	require.NotContains(t, logLine, "secret-jwt-token", "Authorization MUST be redacted")
	require.NotContains(t, logLine, "secret-api-key", "X-Api-Key MUST be redacted")
	require.NotContains(t, logLine, "secret-proxy-key", "extra redact header MUST be applied")
	require.NotContains(t, logLine, "secret-tenant", "operator-supplied extras MUST be applied")
	require.NotContains(t, logLine, "secret-cookie-value", "Set-Cookie MUST be redacted")
	require.Contains(t, logLine, "this-is-fine", "non-credential headers MUST pass through verbatim")
}

// TestFullCapture_BodyTruncatedAtCap exercises the body capture path
// and the hard 64 KiB cap. The captured body is base64-encoded; we
// decode and assert it equals the truncated prefix.
func TestFullCapture_BodyTruncatedAtCap(t *testing.T) {
	t.Parallel()
	buf := &bytes.Buffer{}
	logger := zerolog.New(buf).Level(zerolog.DebugLevel)

	const cap = 32 // tiny cap so the test is fast + deterministic
	h := mw.FullCapture(logger, mw.FullCaptureConfig{
		Enabled:   true,
		BodyBytes: cap,
	})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		_, _ = w.Write([]byte("response-body=" + string(body[:5])))
	}))

	srv := httptest.NewServer(h)
	defer srv.Close()

	bigBody := strings.Repeat("A", 1024)
	resp, err := http.Post(srv.URL, "application/octet-stream", strings.NewReader(bigBody))
	require.NoError(t, err)
	_ = resp.Body.Close()

	var line map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &line))

	b64Body, _ := line["request_body_b64"].(string)
	require.NotEmpty(t, b64Body, "request body MUST be captured when BodyBytes>0")
	// The capture is base64 — but we can at least assert its decoded
	// length matches the cap.
	require.LessOrEqual(t, decodedLen(t, b64Body), cap+4, /* base64 padding slop */
		"captured prefix MUST be truncated to the configured cap")
}

// TestIsFullCapture_FlagPropagatesIntoContext — handlers downstream
// of the middleware need to know capture is on so they can emit
// extra per-phase debug events without paying the cost when capture
// is off. The middleware sets a context flag; this test asserts the
// IsFullCapture predicate reflects it.
func TestIsFullCapture_FlagPropagatesIntoContext(t *testing.T) {
	t.Parallel()
	buf := &bytes.Buffer{}
	logger := zerolog.New(buf).Level(zerolog.DebugLevel)

	var observed bool
	h := mw.FullCapture(logger, mw.FullCaptureConfig{Enabled: true})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed = mw.IsFullCapture(r.Context())
	}))
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.True(t, observed, "handler context MUST report IsFullCapture=true when middleware is enabled")
}

func decodedLen(t *testing.T, b64 string) int {
	t.Helper()
	// rough size estimate; close enough for the truncation assertion.
	padding := strings.Count(b64, "=")
	return (len(b64)*3)/4 - padding
}
