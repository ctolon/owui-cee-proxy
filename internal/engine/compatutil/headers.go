package compatutil

import "net/http"

// inboundHeaderAllowlist is the canonical set of inbound headers
// that adapters AND the passthrough mount may forward to the engine.
// Mirrors docling.fwdHeaderAllowlist (same shape, same intent), but
// available at the compatutil package so the passthrough reverse
// proxy and the handler-side request builder share one source of
// truth.
//
// Adding a header here means: every engine sees it. Don't add
// transport headers (Host, Content-Length, Connection) — net/http
// handles those, and including them would corrupt the outbound
// request shape.
//
// Hop-by-hop and security-sensitive headers (Authorization, Cookie,
// X-Forwarded-*) are INTENTIONALLY excluded; the proxy stamps its
// own auth via authutil.Apply and forwarded-for headers via the
// reverseproxy.New trusted-proxies path.
var inboundHeaderAllowlist = map[string]struct{}{
	"Accept":          {},
	"Accept-Encoding": {},
	"Accept-Language": {},
	"User-Agent":      {},
	"X-Request-Id":    {},
	"Content-Type":    {},
}

// AllowlistedHeaders returns a new http.Header containing only the
// allowlisted keys from src. Faster + less allocation than
// http.Header.Clone() for handlers that already know they don't
// need the full inbound map (engines.ConvertRequest.Headers in
// particular only ever sees the allowlist downstream).
//
// The returned map is safe to mutate independently of src.
func AllowlistedHeaders(src http.Header) http.Header {
	out := make(http.Header, len(inboundHeaderAllowlist))
	for k := range inboundHeaderAllowlist {
		if vs := src.Values(k); len(vs) > 0 {
			cp := make([]string, len(vs))
			copy(cp, vs)
			out[http.CanonicalHeaderKey(k)] = cp
		}
	}
	return out
}
