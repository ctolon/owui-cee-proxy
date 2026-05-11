package middleware

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// requestMeta is a mutable struct stashed in the request context. The
// AccessLog middleware installs it; convert handlers mutate it once
// they have decided which engine handles the request and which file
// is being processed. Because the *pointer* is what travels in the
// context, the middleware sees the handler's updates after ServeHTTP
// returns.
//
// Timing fields:
//
//	upstreamStart  — set by StartUpstream when the adapter dispatches
//	                 to the engine backend
//	upstreamDur    — set by EndUpstream once the adapter has the
//	                 response (or an error); zero means "engine call
//	                 never happened" (request rejected upstream)
//	upstreamStatus — backend HTTP status; 0 means "no response /
//	                 transport error"
//	ttfb           — wall-clock time from the start of AccessLog to
//	                 the first WriteHeader (captures handler time
//	                 spent assembling the response before bytes flow)
type requestMeta struct {
	mu        sync.Mutex
	engine    string
	engineURL string
	filename  string
	mimeType  string
	// mimeSource is one of:
	//   declared | sniffed | sniffed_then_extension | extension | fallback
	// matching the engine.mimedetect.Source set.
	mimeSource     string
	mimeDeclared   string // the original Content-Type the client sent
	pickSource     string // "mime" | "extension" | "default"
	routingStrat   string // "mime" | "extension" | "mime_then_extension" | "extension_then_mime"
	fileCount      int
	upstreamStart  time.Time
	upstreamDur    time.Duration
	upstreamStatus int
	ttfb           time.Duration
}

type metaKey struct{}

func metaFrom(ctx context.Context) *requestMeta {
	if m, ok := ctx.Value(metaKey{}).(*requestMeta); ok {
		return m
	}
	return nil
}

// WithEngine annotates the (already-installed) requestMeta with the
// chosen engine target. Safe to call from the handler goroutine.
func WithEngine(ctx context.Context, name, url string) context.Context {
	if m := metaFrom(ctx); m != nil {
		m.mu.Lock()
		m.engine = name
		m.engineURL = url
		m.mu.Unlock()
	}
	return ctx
}

func WithFilename(ctx context.Context, name string) context.Context {
	if m := metaFrom(ctx); m != nil {
		m.mu.Lock()
		m.filename = name
		m.mu.Unlock()
	}
	return ctx
}

func WithFileCount(ctx context.Context, n int) context.Context {
	if m := metaFrom(ctx); m != nil {
		m.mu.Lock()
		m.fileCount = n
		m.mu.Unlock()
	}
	return ctx
}

// WithMimeType records the inbound Content-Type the proxy picked up
// from the first file part (multipart facade) or from the request
// header (external PUT facade). Surfaced as the `mime_type` field on
// the request_completed access log line and the
// engine_upstream_status / outbound_engine_request debug lines so
// operators can group "this MIME type 4xx'd" without reverse-mapping
// from filename extensions.
func WithMimeType(ctx context.Context, ct string) context.Context {
	if m := metaFrom(ctx); m != nil {
		m.mu.Lock()
		m.mimeType = ct
		m.mu.Unlock()
	}
	return ctx
}

// MimeTypeFrom reads back the recorded MIME type. Used by handlers
// that need to enrich a downstream log line (e.g., upstream-error
// builder).
func MimeTypeFrom(ctx context.Context) string {
	if m := metaFrom(ctx); m != nil {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.mimeType
	}
	return ""
}

// WithMimeSource records (a) how the resolved MIME was chosen
// (`declared` | `sniffed` | `sniffed_then_extension` | `extension` |
// `fallback`) and (b) the original declared value the client sent.
// Both surface as separate access-log fields so an operator can tell
// "client sent application/octet-stream, we sniffed application/pdf,
// routed to docling" in one log line.
func WithMimeSource(ctx context.Context, source, declared string) context.Context {
	if m := metaFrom(ctx); m != nil {
		m.mu.Lock()
		m.mimeSource = source
		m.mimeDeclared = declared
		m.mu.Unlock()
	}
	return ctx
}

// WithPickDecision records WHY a particular engine was picked:
//
//   - source is the matcher that won the dispatch:
//     `mime` | `extension` | `default`.
//   - strategy is the registry's configured routing strategy:
//     `mime` | `extension` | `mime_then_extension` | `extension_then_mime`.
//
// Surfaced as separate access-log fields so operators can answer
// "why did .msg route to kreuzberg?" with a single log record
// (mime_source=sniffed_then_extension pick_source=extension
// routing_strategy=mime_then_extension).
func WithPickDecision(ctx context.Context, source, strategy string) context.Context {
	if m := metaFrom(ctx); m != nil {
		m.mu.Lock()
		m.pickSource = source
		m.routingStrat = strategy
		m.mu.Unlock()
	}
	return ctx
}

// PickDecisionFrom reads back the recorded pick decision (matcher,
// strategy). Used by handlers that want to emit a dedicated
// `engine_pick_decision` debug event alongside the request-scoped
// access log.
func PickDecisionFrom(ctx context.Context) (source, strategy string) {
	if m := metaFrom(ctx); m != nil {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.pickSource, m.routingStrat
	}
	return "", ""
}

// StartUpstream records the moment an adapter begins dispatching to
// its backend. Call it as the LAST thing before client.HTTP.Do (or
// the equivalent) so the recorded duration is the wall-clock cost of
// the network round-trip + backend processing — not the time spent
// assembling the request body.
func StartUpstream(ctx context.Context) {
	if m := metaFrom(ctx); m != nil {
		m.mu.Lock()
		m.upstreamStart = time.Now()
		m.mu.Unlock()
	}
}

// EndUpstream records the response status and elapsed time since the
// matching StartUpstream call. status=0 signals a transport-level
// failure (no HTTP response). Safe to call without a prior
// StartUpstream — the duration is zero in that case.
func EndUpstream(ctx context.Context, status int) {
	if m := metaFrom(ctx); m != nil {
		m.mu.Lock()
		if !m.upstreamStart.IsZero() {
			m.upstreamDur = time.Since(m.upstreamStart)
		}
		m.upstreamStatus = status
		m.mu.Unlock()
	}
}

// UpstreamStatusFrom returns the backend HTTP status recorded by the
// adapter via EndUpstream, or 0 if no upstream call occurred (or it
// failed before producing a response). Used by handlers that need to
// decide whether to emit an "upstream error" log line.
func UpstreamStatusFrom(ctx context.Context) int {
	if m := metaFrom(ctx); m != nil {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.upstreamStatus
	}
	return 0
}

// UpstreamDurationFrom returns the wall-clock duration of the most
// recent engine call (StartUpstream → EndUpstream window), or 0 if
// no upstream call occurred.
func UpstreamDurationFrom(ctx context.Context) time.Duration {
	if m := metaFrom(ctx); m != nil {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.upstreamDur
	}
	return 0
}

// silenceLogKey is the context key used by readiness/health handlers
// to ask downstream observability layers (AccessLog, instrumented
// HTTP transport) to drop their per-request log line. Metrics are
// NEVER suppressed by this flag — only the verbose logs are.
type silenceLogKey struct{}

// WithSilenceLog returns a ctx whose downstream log emitters skip
// emission. Use in the readiness/health handler to mute the engine
// health probes that fire every few seconds when k8s polls /readyz.
func WithSilenceLog(ctx context.Context) context.Context {
	return context.WithValue(ctx, silenceLogKey{}, true)
}

// IsLogSilenced reports whether ctx is marked silent. Both AccessLog
// and InstrumentedTransport consult this before emitting their
// per-request log lines. Defensive against nil ctx.
func IsLogSilenced(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(silenceLogKey{}).(bool)
	return v
}

func engineFrom(ctx context.Context) (string, string) {
	m := metaFrom(ctx)
	if m == nil {
		return "", ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.engine, m.engineURL
}

// AccessLog emits a structured stdout log entry for every completed
// request. The engine + filename fields fulfil the project requirement
// "stdouttan hangi dosya hangi engine'e yönlendirilmiş anlayabilmeliyim".
//
// Emitted timing fields (all milliseconds, rounded to nearest int):
//
//	total_ms             — wall-clock from middleware entry to handler return
//	time_to_first_byte_ms — middleware entry → first WriteHeader/Write
//	upstream_ms          — adapter's StartUpstream → EndUpstream window
//	                       (0 if no upstream call took place)
//	upstream_status      — backend HTTP status (0 = no response)
//
// silencePaths is the operator-supplied allowlist of paths whose
// per-request log lines should be dropped (typical: /healthz, /readyz,
// /metrics). The middleware still installs requestMeta and tracks
// timing fields — only the final emission is skipped — so a handler
// that opts into emission later (via WithSilenceLog inverted) still
// works downstream. Compared by exact path match.
func AccessLog(logger zerolog.Logger, silencePaths ...string) func(http.Handler) http.Handler {
	silenceSet := make(map[string]struct{}, len(silencePaths))
	for _, p := range silencePaths {
		if p != "" {
			silenceSet[p] = struct{}{}
		}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			meta := &requestMeta{}
			ctx := context.WithValue(r.Context(), metaKey{}, meta)
			r = r.WithContext(ctx)

			start := time.Now()
			rw := &responseWriter{
				ResponseWriter: w,
				status:         http.StatusOK,
				start:          start,
				meta:           meta,
			}
			next.ServeHTTP(rw, r)
			total := time.Since(start)

			// Suppression: either the path is in the operator's mute
			// list, or a handler explicitly marked this ctx silent
			// (typically the readiness handler wrapping engine probes).
			if _, muted := silenceSet[r.URL.Path]; muted || IsLogSilenced(r.Context()) {
				return
			}

			meta.mu.Lock()
			engineName, engineURL := meta.engine, meta.engineURL
			mimeSource, mimeDeclared := meta.mimeSource, meta.mimeDeclared
			pickSource, routingStrat := meta.pickSource, meta.routingStrat
			filename, fileCount := meta.filename, meta.fileCount
			mimeType := meta.mimeType
			upstreamDur, upstreamStatus := meta.upstreamDur, meta.upstreamStatus
			ttfb := meta.ttfb
			meta.mu.Unlock()

			ev := logger.Info()
			if rw.status >= 500 {
				ev = logger.Error()
			} else if rw.status >= 400 {
				ev = logger.Warn()
			}
			ev.
				Str("event", "request_completed").
				Str("request_id", IDFrom(r.Context())).
				Str("method", r.Method).
				Str("path", r.URL.Path).
				Int("status", rw.status).
				Int64("bytes_out", rw.bytes).
				Dur("duration", total).
				Int64("total_ms", total.Milliseconds()).
				Int64("time_to_first_byte_ms", ttfb.Milliseconds()).
				Int64("upstream_ms", upstreamDur.Milliseconds()).
				Int("upstream_status", upstreamStatus).
				Str("remote_addr", r.RemoteAddr).
				Str("engine", engineName).
				Str("engine_url", engineURL).
				Str("filename", filename).
				Str("mime_type", mimeType).
				Str("mime_source", mimeSource).
				Str("mime_declared", mimeDeclared).
				Str("pick_source", pickSource).
				Str("routing_strategy", routingStrat).
				Int("file_count", fileCount).
				Msg("request_completed")
		})
	}
}

// responseWriter wraps the underlying http.ResponseWriter so the
// middleware stack can observe status code and response byte count.
//
// Concurrency: in normal handlers Write/WriteHeader are called
// sequentially from one goroutine, but a misbehaving handler (or one
// using net/http's panic-recover semantics) may invoke WriteHeader
// twice. We use sync.Once to guarantee the underlying writer's
// WriteHeader is called exactly once even under that race; subsequent
// calls are silently dropped (matching http.ResponseWriter contract).
type responseWriter struct {
	http.ResponseWriter
	status    int
	bytes     int64
	writeOnce sync.Once
	// start + meta let the writer record the time-to-first-byte
	// against the parent requestMeta without forcing handlers to
	// thread a separate "I'm about to write" hook.
	start time.Time
	meta  *requestMeta
}

// recordTTFB writes the time-to-first-byte into the request meta the
// first time it's called. Defensive against missing meta (legacy
// tests that build a responseWriter without going through AccessLog).
func (r *responseWriter) recordTTFB() {
	if r.meta == nil || r.start.IsZero() {
		return
	}
	r.meta.mu.Lock()
	if r.meta.ttfb == 0 {
		r.meta.ttfb = time.Since(r.start)
	}
	r.meta.mu.Unlock()
}

func (r *responseWriter) WriteHeader(status int) {
	r.writeOnce.Do(func() {
		r.status = status
		r.recordTTFB()
		r.ResponseWriter.WriteHeader(status)
	})
}

func (r *responseWriter) Write(b []byte) (int, error) {
	// http.ResponseWriter contract: implicit 200 if Write is called
	// before WriteHeader. Mark the header as committed so a later
	// (incorrect) WriteHeader call doesn't try to overwrite it.
	r.writeOnce.Do(func() {
		// Status stays at the default (200) set in the constructor.
		r.recordTTFB()
	})
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

// Flush passthrough for streaming responses.
func (r *responseWriter) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack passthrough for websockets and other connection-takeover
// patterns. If the wrapped writer doesn't implement http.Hijacker we
// return a clear error rather than letting the type assertion panic
// inside the caller — net/http's default ResponseWriter implementation
// implements Hijacker, but TimeoutHandler / mock writers may not.
func (r *responseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("middleware: underlying ResponseWriter does not support Hijacker")
	}
	return hj.Hijack()
}

// Push passthrough so HTTP/2 server-push handlers can reach the
// underlying writer. Returns http.ErrNotSupported when unsupported,
// matching the http.Pusher contract.
func (r *responseWriter) Push(target string, opts *http.PushOptions) error {
	if p, ok := r.ResponseWriter.(http.Pusher); ok {
		return p.Push(target, opts)
	}
	return http.ErrNotSupported
}
