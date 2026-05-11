package middleware

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"strings"

	"github.com/rs/zerolog"
)

// FullCaptureConfig drives the optional audit-capture middleware.
// Default zero value is OFF — capture is a deliberate operator opt-in
// gated by the global Observability.Upstream.FullCapture knob (and
// the per-engine override resolved at handler time).
//
// SENSITIVE BY DESIGN: capture emits the inbound request + response
// header set and (when BodyBytes>0) a base64 prefix of the body. The
// log line carries `sensitive_capture: true` so shippers can route
// it separately. Header values for known credentials are redacted
// IN PLACE before logging:
//
//   - Authorization
//   - Cookie / Set-Cookie
//   - Proxy-Authorization
//   - the configured ProxyAPIKeyHeader
//   - every value of RedactExtraHeaders (engine auth header names)
//
// Body redaction is OUT OF SCOPE here — the middleware captures the
// raw prefix and trusts the operator's downstream PII pipeline. The
// 64 KiB hard cap in the config validator bounds the worst case.
type FullCaptureConfig struct {
	// Enabled toggles the capture. False = middleware is a no-op
	// (zero-cost pass-through). When true, every request through the
	// wrapped chain emits one `request_full_capture` debug-level log
	// line, plus a same-id pairing line if the request errored.
	Enabled bool
	// BodyBytes caps the inbound + outbound body prefix written to the
	// log. 0 disables body capture entirely (headers only). The chain
	// enforces a 64 KiB ceiling regardless of operator setting.
	BodyBytes int
	// RedactExtraHeaders is the case-insensitive set of additional
	// header names whose values should be replaced with
	// "<redacted len=N>" before logging. The composition root passes
	// every engine's configured auth_header name plus the global
	// ProxyAPIKeyHeader.
	RedactExtraHeaders []string
}

// hard cap matches config validator (lte=65536). Even if the
// operator sets FullCaptureBodyBytes higher, the chain truncates
// here so the worst-case log line is bounded.
const fullCaptureBodyHardCap = 64 << 10

// builtinRedactedHeaders is the always-on credential allowlist.
// Lowercased; lookups use case-folded comparison.
var builtinRedactedHeaders = map[string]struct{}{
	"authorization":       {},
	"proxy-authorization": {},
	"cookie":              {},
	"set-cookie":          {},
	"x-api-key":           {},
}

// FullCapture wraps next to optionally emit a request_full_capture
// log record. When cfg.Enabled is false, returns next unchanged
// (compile-time zero overhead).
func FullCapture(logger zerolog.Logger, cfg FullCaptureConfig) func(http.Handler) http.Handler {
	if !cfg.Enabled {
		return func(next http.Handler) http.Handler { return next }
	}
	redactSet := buildRedactSet(cfg.RedactExtraHeaders)
	bodyCap := cfg.BodyBytes
	if bodyCap > fullCaptureBodyHardCap {
		bodyCap = fullCaptureBodyHardCap
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r = r.WithContext(WithFullCapture(r.Context()))

			var reqBody string
			if bodyCap > 0 && r.Body != nil {
				reqBody, r.Body = peekBody(r.Body, bodyCap)
			}

			cw := &captureResponseWriter{
				ResponseWriter: w,
				bodyCap:        bodyCap,
				body:           &bytes.Buffer{},
			}
			next.ServeHTTP(cw, r)

			logger.Debug().
				Bool("sensitive_capture", true).
				Str("event", "request_full_capture").
				Str("request_id", IDFrom(r.Context())).
				Str("method", r.Method).
				Str("path", r.URL.Path).
				Str("query", r.URL.RawQuery).
				Str("remote_addr", r.RemoteAddr).
				Int("status", cw.status).
				Int64("response_bytes", cw.totalBytes).
				Interface("request_headers", redactHeaders(r.Header, redactSet)).
				Interface("response_headers", redactHeaders(cw.Header(), redactSet)).
				Str("request_body_b64", reqBody).
				Str("response_body_b64", encodeBody(cw.body.Bytes())).
				Msg("full request/response capture")
		})
	}
}

func buildRedactSet(extra []string) map[string]struct{} {
	set := make(map[string]struct{}, len(builtinRedactedHeaders)+len(extra))
	for k := range builtinRedactedHeaders {
		set[k] = struct{}{}
	}
	for _, h := range extra {
		if h == "" {
			continue
		}
		set[strings.ToLower(h)] = struct{}{}
	}
	return set
}

// redactHeaders returns a copy of src with credential values
// replaced by "<redacted len=N>" so the operator can correlate
// presence without reading the secret value. The header NAMES are
// kept intact — only values are scrubbed.
func redactHeaders(src http.Header, redactSet map[string]struct{}) map[string][]string {
	out := make(map[string][]string, len(src))
	for k, vs := range src {
		if _, redact := redactSet[strings.ToLower(k)]; redact {
			redacted := make([]string, len(vs))
			for i, v := range vs {
				redacted[i] = "<redacted len=" + itoa(len(v)) + ">"
			}
			out[k] = redacted
			continue
		}
		cp := make([]string, len(vs))
		copy(cp, vs)
		out[k] = cp
	}
	return out
}

// itoa avoids strconv on the redaction hot path while keeping logs
// reproducible.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := [20]byte{}
	i := len(digits)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		digits[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		digits[i] = '-'
	}
	return string(digits[i:])
}

// peekBody buffers the first n bytes of body for the capture log,
// then returns a teed reader whose downstream reads see the full
// original stream. The caller is responsible for closing the
// returned io.ReadCloser; we wrap the original close.
func peekBody(body io.ReadCloser, n int) (b64Prefix string, restored io.ReadCloser) {
	prefix := &bytes.Buffer{}
	limited := io.LimitReader(body, int64(n))
	_, _ = io.Copy(prefix, limited)
	rest := io.MultiReader(bytes.NewReader(prefix.Bytes()), body)
	return encodeBody(prefix.Bytes()), readCloser{r: rest, c: body}
}

// encodeBody returns base64 of the first n bytes (or empty when n=0
// / body absent). Keeps the log line binary-safe.
func encodeBody(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return base64.StdEncoding.EncodeToString(b)
}

type readCloser struct {
	r io.Reader
	c io.Closer
}

func (rc readCloser) Read(p []byte) (int, error) { return rc.r.Read(p) }
func (rc readCloser) Close() error               { return rc.c.Close() }

// captureResponseWriter mirrors what the AccessLog responseWriter
// does (status + byte count) plus a bounded copy of the body prefix
// for the audit line.
type captureResponseWriter struct {
	http.ResponseWriter
	status     int
	totalBytes int64
	body       *bytes.Buffer
	bodyCap    int
}

func (c *captureResponseWriter) WriteHeader(status int) {
	c.status = status
	c.ResponseWriter.WriteHeader(status)
}

func (c *captureResponseWriter) Write(p []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	if c.bodyCap > 0 && c.body.Len() < c.bodyCap {
		remaining := c.bodyCap - c.body.Len()
		if remaining > len(p) {
			remaining = len(p)
		}
		c.body.Write(p[:remaining])
	}
	n, err := c.ResponseWriter.Write(p)
	c.totalBytes += int64(n)
	return n, err
}

// Flush passthrough for SSE-style handlers.
func (c *captureResponseWriter) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// captureKey is a context flag handlers can check to decide whether
// a per-handler debug snapshot is worth the cost — when capture is
// off, even the most diligent handler should skip its deep-dive
// logs.
type captureKey struct{}

// WithFullCapture marks ctx as capture-on. Handler code reads via
// IsFullCapture to gate any expensive structured-dump branches.
func WithFullCapture(ctx context.Context) context.Context {
	return context.WithValue(ctx, captureKey{}, true)
}

// IsFullCapture reports whether the request is being captured. Used
// by handlers and adapters to decide whether to emit additional
// per-phase debug events.
func IsFullCapture(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(captureKey{}).(bool)
	return v
}
