package middleware

import (
	"context"
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

type responseWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
	wrote  bool
}

func (r *responseWriter) WriteHeader(status int) {
	if r.wrote {
		return
	}
	r.status = status
	r.wrote = true
	r.ResponseWriter.WriteHeader(status)
}

func (r *responseWriter) Write(b []byte) (int, error) {
	if !r.wrote {
		r.wrote = true
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

// Hijacker / Flusher passthroughs for streaming + websocket.
func (r *responseWriter) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
