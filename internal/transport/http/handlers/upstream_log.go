package handlers

import (
	"bytes"
	"context"
	"io"
	"regexp"
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
// from binary-content backends would otherwise produce invalid JSON);
// known credential markers (authorization, api_key, bearer, token,
// password, secret followed by the obvious assignment punctuation) are
// further redacted so an upstream that echoes the inbound request, or
// returns its own credential material in a debug error, does NOT leak
// secrets into the operator-facing log channel.
func sanitizeForLog(b []byte) string {
	utf := utf8String(b)
	return redactCredentialFragments(utf)
}

// utf8String mirrors the original sanitizeForLog body — return as-is
// when valid, strip RuneError runes otherwise.
func utf8String(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
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

// Credential redaction is layered over three regex passes — the
// shapes differ enough that one expression would be unreadable.
// Each pass strips a different class of leak; replacements use
// `***REDACTED***` so an operator can still audit that a secret
// WAS present, just not what it was.
//
//  1. jsonStringKVRE — JSON-style `"keyword": "value"` (the most
//     common shape; engines echoing inbound config / request bodies).
//  2. bearerTokenRE — `Authorization: <anything>` or inline
//     `Bearer <token>`; covers HTTP-header echoes in error messages.
//  3. genericKVRE — bare `keyword=value` or `keyword: value` (urlencoded,
//     YAML, TOML, ad-hoc log fragments).
//
// Keyword set is the OWASP top: authorization, api_key (with
// dash/underscore variants), bearer, token, password, secret.
// Operators with unusual credential names should configure their
// log shipper as defense-in-depth rather than widen this list past
// the point of false positives on ordinary "code" / "type" fields.
var (
	jsonStringKVRE = regexp.MustCompile(
		`(?i)"(authorization|api[_-]?key|bearer|token|password|secret)"\s*:\s*"[^"]*"`,
	)
	bearerTokenRE = regexp.MustCompile(
		`(?i)(authorization\s*:\s*[^\r\n"]+|\bbearer\s+[^\s"',;}\r\n]+(?:\.[^\s"',;}\r\n]+)*)`,
	)
	genericKVRE = regexp.MustCompile(
		`(?i)(api[_-]?key|token|password|secret)\s*[:=]\s*"?([^\s"',;}]+)"?`,
	)
)

func redactCredentialFragments(s string) string {
	s = jsonStringKVRE.ReplaceAllStringFunc(s, func(match string) string {
		// Find the `:` separating key from value; redact the value.
		sub := jsonStringKVRE.FindStringSubmatch(match)
		if len(sub) < 2 {
			return match
		}
		colon := strings.Index(match, ":")
		if colon < 0 {
			return match
		}
		return match[:colon+1] + `"***REDACTED***"`
	})
	s = bearerTokenRE.ReplaceAllStringFunc(s, func(match string) string {
		// Two shapes: "Authorization: X" → "Authorization: ***REDACTED***";
		// "Bearer X.Y.Z" → "Bearer ***REDACTED***".
		lower := strings.ToLower(match)
		if strings.HasPrefix(lower, "authorization") {
			colon := strings.Index(match, ":")
			if colon >= 0 {
				return match[:colon+1] + " ***REDACTED***"
			}
		}
		// Bearer-shaped: keep the literal "Bearer" prefix from the
		// match's casing for auditability.
		space := strings.IndexAny(match, " \t")
		if space >= 0 {
			return match[:space+1] + "***REDACTED***"
		}
		return "***REDACTED***"
	})
	s = genericKVRE.ReplaceAllStringFunc(s, func(match string) string {
		sub := genericKVRE.FindStringSubmatch(match)
		if len(sub) < 3 {
			return match
		}
		idx := strings.Index(match, sub[2])
		if idx < 0 {
			return match
		}
		return match[:idx] + "***REDACTED***"
	})
	return s
}
