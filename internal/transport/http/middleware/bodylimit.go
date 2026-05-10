package middleware

import (
	"context"
	"io"
	"net/http"
	"sync/atomic"
)

type bodyBytesKey struct{}

// bodyBytesIn returns the number of inbound bytes read so far for the
// request, or 0 if no counter is installed (BodyLimit not in chain).
func bodyBytesIn(ctx context.Context) int64 {
	if c, ok := ctx.Value(bodyBytesKey{}).(*int64); ok && c != nil {
		return atomic.LoadInt64(c)
	}
	return 0
}

// BodyLimit caps the request body to maxBytes via http.MaxBytesReader.
// Requests that exceed the cap fail with 413 when the body is read.
//
// In addition to enforcing the cap, BodyLimit wraps the body in a
// counting reader and stashes the counter in the request context so
// the Metrics middleware can later record `body_bytes{direction=in}`
// without buffering the body.
func BodyLimit(maxBytes int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body := r.Body
			if maxBytes > 0 {
				body = http.MaxBytesReader(w, body, maxBytes)
			}
			counter := new(int64)
			r.Body = &countingBody{ReadCloser: body, n: counter}
			ctx := context.WithValue(r.Context(), bodyBytesKey{}, counter)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// countingBody is a wrapper around the request body that increments a
// shared int64 atomically. Pointer + atomic is used so the metrics
// middleware can safely read the running total even if a handler is
// streaming the body in another goroutine (rare but possible with
// hijack/pipe patterns).
type countingBody struct {
	io.ReadCloser
	n *int64
}

func (c *countingBody) Read(p []byte) (int, error) {
	read, err := c.ReadCloser.Read(p)
	if read > 0 {
		atomic.AddInt64(c.n, int64(read))
	}
	return read, err
}
