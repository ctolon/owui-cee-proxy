package middleware

import (
	"crypto/subtle"
	"net/http"
)

// APIKey enforces presence of one of the configured proxy API keys in
// the request header. Comparison is constant-time. When required is
// false this middleware is a no-op (used for environments where auth
// is enforced one layer up).
func APIKey(headerName string, allowed []string, required bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !required {
				next.ServeHTTP(w, r)
				return
			}
			provided := []byte(r.Header.Get(headerName))
			if len(provided) == 0 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			ok := false
			for _, k := range allowed {
				if subtle.ConstantTimeCompare(provided, []byte(k)) == 1 {
					ok = true
					break
				}
			}
			if !ok {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
