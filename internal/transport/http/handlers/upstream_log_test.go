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
