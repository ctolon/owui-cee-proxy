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
type requestMeta struct {
	mu        sync.Mutex
	engine    string
	engineURL string
	filename  string
	fileCount int
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
func AccessLog(logger zerolog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			meta := &requestMeta{}
			ctx := context.WithValue(r.Context(), metaKey{}, meta)
			r = r.WithContext(ctx)

			start := time.Now()
			rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rw, r)

			meta.mu.Lock()
			engineName, engineURL := meta.engine, meta.engineURL
			filename, fileCount := meta.filename, meta.fileCount
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
				Dur("duration", time.Since(start)).
				Str("remote_addr", r.RemoteAddr).
				Str("engine", engineName).
				Str("engine_url", engineURL).
				Str("filename", filename).
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
}

func (r *responseWriter) WriteHeader(status int) {
	r.writeOnce.Do(func() {
		r.status = status
		r.ResponseWriter.WriteHeader(status)
	})
}

func (r *responseWriter) Write(b []byte) (int, error) {
	// http.ResponseWriter contract: implicit 200 if Write is called
	// before WriteHeader. Mark the header as committed so a later
	// (incorrect) WriteHeader call doesn't try to overwrite it.
	r.writeOnce.Do(func() {
		// Status stays at the default (200) set in the constructor.
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
