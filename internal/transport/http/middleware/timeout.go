package middleware

import (
	"context"
	"net/http"
	"time"
)

// Timeout adds a request-scoped deadline. Unlike http.TimeoutHandler
// it does not interfere with streaming responses; instead it relies
// on downstream handlers honoring r.Context().
func Timeout(d time.Duration) func(http.Handler) http.Handler {
	if d <= 0 {
		return func(h http.Handler) http.Handler { return h }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
