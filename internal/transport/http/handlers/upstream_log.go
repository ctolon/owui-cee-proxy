package handlers

import (
	"bytes"
	"context"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/rs/zerolog"

	"github.com/ctolon/owui-cee-proxy/internal/config"
	"github.com/ctolon/owui-cee-proxy/internal/engine"
	mw "github.com/ctolon/owui-cee-proxy/internal/transport/http/middleware"
)

// upstreamStatusEvent is the canonical event name for engine 4xx/5xx
// log lines. Operators key dashboards / alerts off this string.
const upstreamStatusEvent = "engine_upstream_status"

// logUpstreamStatus emits the per-request upstream-error log line and
// rewinds the response body so the caller's io.Copy still sees the
// full bytes. The body Peek is bounded by cfg.BodySnippetBytes (0 =
// no snippet); a non-zero snippet is captured into a bytes.Buffer and
// re-prepended to the original ReadCloser.
//
// Returns the (possibly rewound) response so the caller chains it
// back into engine.ConvertResponse. resp.StatusCode is unchanged.
func logUpstreamStatus(
	ctx context.Context,
	logger zerolog.Logger,
	r *engine.ConvertResponse,
	cfg config.UpstreamLogConfig,
	engineName, engineURL, requestID, filename string,
) *engine.ConvertResponse {
	if r == nil || !cfg.Enabled || r.StatusCode < 400 {
		return r
	}

	level := cfg.Level5xx
	if r.StatusCode < 500 {
		level = cfg.Level4xx
	}

	var snippet string
	if cfg.BodySnippetBytes > 0 && r.Body != nil {
		buf := bytes.NewBuffer(make([]byte, 0, cfg.BodySnippetBytes))
		// Read at most BodySnippetBytes; preserve whatever's left.
		_, _ = io.CopyN(buf, r.Body, int64(cfg.BodySnippetBytes))
		snippet = sanitizeForLog(buf.Bytes())
		// Re-prepend the captured prefix so the downstream caller
		// still receives the full body.
		r.Body = struct {
			io.Reader
			io.Closer
		}{
			Reader: io.MultiReader(bytes.NewReader(buf.Bytes()), r.Body),
			Closer: r.Body,
		}
	}

	ev := eventForLevel(logger, level, r.StatusCode)
	ev.
		Str("event", upstreamStatusEvent).
		Str("request_id", requestID).
		Str("engine", engineName).
		Str("engine_url", engineURL).
		Str("filename", filename).
		Str("mime_type", mw.MimeTypeFrom(ctx)).
		Int("upstream_status", r.StatusCode).
		Int64("upstream_ms", mw.UpstreamDurationFrom(ctx).Milliseconds()).
		Str("body_snippet", snippet).
		Msg("engine returned non-2xx status")

	return r
}

// eventForLevel maps the configured level string to a zerolog.Event.
// Unknown strings fall back to a sensible default keyed off the
// HTTP status class (4xx→warn, 5xx→error) so a misspelled config
// never silently drops a log line.
func eventForLevel(logger zerolog.Logger, level string, status int) *zerolog.Event {
	switch strings.ToLower(level) {
	case "trace":
		return logger.Trace()
	case "debug":
		return logger.Debug()
	case "info":
		return logger.Info()
	case "warn", "warning":
		return logger.Warn()
	case "error":
		return logger.Error()
	}
	if status >= 500 {
		return logger.Error()
	}
	return logger.Warn()
}

// sanitizeForLog returns a valid-UTF-8 representation safe for
// zerolog's JSON encoder. Non-UTF8 bytes are dropped (response bodies
// from binary-content backends would otherwise produce invalid JSON).
func sanitizeForLog(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	// Strip invalid runes; this is a debug field, fidelity not
	// critical.
	v := make([]rune, 0, len(b))
	for i := 0; i < len(b); {
		r, size := utf8.DecodeRune(b[i:])
		if r != utf8.RuneError {
			v = append(v, r)
		}
		if size == 0 {
			break
		}
		i += size
	}
	return string(v)
}
