package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/ctolon/owui-cee-proxy/internal/config"
	"github.com/ctolon/owui-cee-proxy/internal/engine"
)

func newResp(status int, body string) *engine.ConvertResponse {
	return &engine.ConvertResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestLogUpstreamStatus_DisabledIsNoOp(t *testing.T) {
	t.Parallel()
	buf := &bytes.Buffer{}
	logger := zerolog.New(buf)
	resp := newResp(500, "boom")
	cfg := config.UpstreamLogConfig{Enabled: false}

	got := logUpstreamStatus(context.Background(), logger, resp, cfg, "eng", "url", "req-1", "f.pdf")
	require.Same(t, resp, got)
	require.Empty(t, buf.String(), "no log line when disabled")
}

func TestLogUpstreamStatus_2xxIsNotLogged(t *testing.T) {
	t.Parallel()
	buf := &bytes.Buffer{}
	logger := zerolog.New(buf)
	resp := newResp(200, "ok")
	cfg := config.UpstreamLogConfig{Enabled: true, BodySnippetBytes: 32, Level4xx: "warn", Level5xx: "error"}

	_ = logUpstreamStatus(context.Background(), logger, resp, cfg, "eng", "url", "r", "f.pdf")
	require.Empty(t, buf.String(), "2xx must not trigger upstream-error log")
}

func TestLogUpstreamStatus_4xxAndBodyPeekRestored(t *testing.T) {
	t.Parallel()
	buf := &bytes.Buffer{}
	logger := zerolog.New(buf).Level(zerolog.DebugLevel)
	resp := newResp(422, `{"detail":"validation failed: bad mime"}`)
	cfg := config.UpstreamLogConfig{Enabled: true, BodySnippetBytes: 64, Level4xx: "warn", Level5xx: "error"}

	got := logUpstreamStatus(context.Background(), logger, resp, cfg, "main-docling", "http://docling:5001", "req-1", "f.pdf")
	require.NotNil(t, got)

	// The log line must surface the status + snippet + engine fields.
	var line map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &line))
	require.Equal(t, upstreamStatusEvent, line["event"])
	require.Equal(t, "warn", line["level"], "4xx defaults to warn level")
	require.Equal(t, "main-docling", line["engine"])
	require.Equal(t, float64(422), line["upstream_status"])
	require.Contains(t, line["body_snippet"].(string), "validation failed")

	// The body must still be readable in full by a downstream io.Copy.
	rest, err := io.ReadAll(got.Body)
	require.NoError(t, err)
	require.Contains(t, string(rest), "validation failed: bad mime")
}

func TestLogUpstreamStatus_5xxUsesErrorLevel(t *testing.T) {
	t.Parallel()
	buf := &bytes.Buffer{}
	logger := zerolog.New(buf).Level(zerolog.DebugLevel)
	resp := newResp(503, "service unavailable")
	cfg := config.UpstreamLogConfig{Enabled: true, BodySnippetBytes: 64, Level4xx: "warn", Level5xx: "error"}

	_ = logUpstreamStatus(context.Background(), logger, resp, cfg, "eng", "url", "r", "f")
	var line map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &line))
	require.Equal(t, "error", line["level"])
}

func TestLogUpstreamStatus_PerEngineLevelOverride(t *testing.T) {
	t.Parallel()
	buf := &bytes.Buffer{}
	logger := zerolog.New(buf).Level(zerolog.DebugLevel)
	// Operator dials 4xx down to info because the backend is flaky.
	cfg := config.UpstreamLogConfig{Enabled: true, BodySnippetBytes: 0, Level4xx: "info", Level5xx: "error"}

	_ = logUpstreamStatus(context.Background(), logger, newResp(404, "nope"), cfg, "e", "u", "r", "f")
	var line map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &line))
	require.Equal(t, "info", line["level"], "operator-configured Level4xx must be honoured")
}

// TestLogUpstreamStatus_BodySnippetRedactsCredentials pins the C-5
// fix: when an upstream echoes the inbound Authorization / api_key /
// password / bearer / token / secret marker into its error body,
// sanitizeForLog scrubs the value so the operator-facing log line
// never carries the secret. The marker (and its assignment
// punctuation) is preserved for auditability — only the value
// becomes "***REDACTED***".
func TestLogUpstreamStatus_BodySnippetRedactsCredentials(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
		// payload is the literal token a regression would leak.
		payload string
	}{
		{"json api_key", `{"error":"bad","api_key":"sk-superSecret123"}`, "sk-superSecret123"},
		{"json Authorization echo", `{"detail":"auth failed Authorization: Bearer abc.def.ghi"}`, "abc.def.ghi"},
		{"urlencoded api-key", "msg=fail&api-key=plainKeyXY&z=1", "plainKeyXY"},
		{"yaml-ish password", `password: hunter2`, "hunter2"},
		{"toml token", `token = "tok_alpha"`, "tok_alpha"},
		{"case-insensitive Secret", `SECRET=topSecretValue`, "topSecretValue"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			buf := &bytes.Buffer{}
			logger := zerolog.New(buf).Level(zerolog.DebugLevel)
			cfg := config.UpstreamLogConfig{Enabled: true, BodySnippetBytes: 1024, Level4xx: "warn", Level5xx: "error"}

			_ = logUpstreamStatus(context.Background(), logger, newResp(500, c.body), cfg, "e", "u", "r", "f")

			require.NotContains(t, buf.String(), c.payload, "credential payload MUST NOT survive into the log line")
			require.Contains(t, buf.String(), "***REDACTED***",
				"redaction marker MUST appear in place of the credential")
		})
	}
}

// TestLogUpstreamStatus_BodySnippetPreservesInnocuousContent
// asserts the redactor is precise — non-credential JSON survives
// the pass unchanged. The literal "code" in "error_code" is one
// the older regex would have mishandled if it had matched on
// "code" alone; we want to be sure we did NOT widen the keyword
// list so aggressively that ordinary error bodies get scrubbed.
func TestLogUpstreamStatus_BodySnippetPreservesInnocuousContent(t *testing.T) {
	t.Parallel()
	buf := &bytes.Buffer{}
	logger := zerolog.New(buf).Level(zerolog.DebugLevel)
	cfg := config.UpstreamLogConfig{Enabled: true, BodySnippetBytes: 1024, Level4xx: "warn", Level5xx: "error"}

	body := `{"error_code":"E_INVALID","detail":"unsupported language en-XX"}`
	_ = logUpstreamStatus(context.Background(), logger, newResp(400, body), cfg, "e", "u", "r", "f")
	require.Contains(t, buf.String(), "unsupported language en-XX",
		"non-credential body content MUST flow through verbatim")
	require.NotContains(t, buf.String(), "***REDACTED***",
		"redactor MUST NOT scrub ordinary error bodies")
}
