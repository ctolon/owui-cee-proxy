package middleware

import (
	"context"
	"net/http"

	"github.com/oklog/ulid/v2"
)

const RequestIDHeader = "X-Request-ID"

type requestIDKey struct{}

// RequestID assigns a ULID to every request that does not already
// carry an X-Request-ID header. The ID is stored in the context and
// echoed back in the response header.
func RequestID() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get(RequestIDHeader)
			if id == "" {
				id = ulid.Make().String()
			}
			ctx := context.WithValue(r.Context(), requestIDKey{}, id)
			w.Header().Set(RequestIDHeader, id)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func IDFrom(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey{}).(string); ok {
		return v
	}
	return ""
}
