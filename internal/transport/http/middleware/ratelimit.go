package middleware

import (
	"net/http"

	"golang.org/x/time/rate"
)

// GlobalRateLimit applies a single token-bucket limiter to all incoming
// requests. Per-engine limiters live on the engine adapter side so they
// can apply during async dispatch as well.
func GlobalRateLimit(rps float64, burst int) func(http.Handler) http.Handler {
	if rps <= 0 || burst <= 0 {
		return func(h http.Handler) http.Handler { return h }
	}
	limiter := rate.NewLimiter(rate.Limit(rps), burst)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !limiter.Allow() {
				http.Error(w, "rate limited", http.StatusTooManyRequests)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
