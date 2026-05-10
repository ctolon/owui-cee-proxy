package middleware

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
)

// APIKey enforces presence of one of the configured proxy API keys in
// the request header. When required is false this middleware is a
// no-op (used for environments where auth is enforced one layer up).
//
// L1: comparing the raw provided key against allowlisted keys with
// subtle.ConstantTimeCompare still leaks the key length (the function
// returns 0 when len(a) != len(b) without doing the constant-time
// loop). To avoid that, we hash both sides to a fixed 32-byte SHA-256
// digest first and compare those. This is purely a side-channel
// defense — there is no goal of cryptographic key derivation here.
func APIKey(headerName string, allowed []string, required bool) func(http.Handler) http.Handler {
	// Pre-hash the allowlist so we don't recompute on every request.
	allowedHashes := make([][32]byte, len(allowed))
	for i, k := range allowed {
		allowedHashes[i] = sha256.Sum256([]byte(k))
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !required {
				next.ServeHTTP(w, r)
				return
			}
			provided := r.Header.Get(headerName)
			if len(provided) == 0 || len(allowedHashes) == 0 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			providedHash := sha256.Sum256([]byte(provided))
			ok := false
			for i := range allowedHashes {
				// Compare the two 32-byte digests in constant time. No
				// length leak because both sides are always 32 bytes.
				if subtle.ConstantTimeCompare(providedHash[:], allowedHashes[i][:]) == 1 {
					ok = true
					// Don't break early — continue iterating so total
					// time does not depend on which slot matched.
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
