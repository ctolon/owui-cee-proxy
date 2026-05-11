package handlers

import (
	"net/http"
	"strings"
)

// outboundResponseHeaderAllowlist is the canonical set of upstream
// response headers we forward back to the client. Anything not on
// this list is dropped — engine backends sometimes echo `Server`,
// `X-Powered-By`, internal correlation IDs, or even `Set-Cookie`
// (Tika's debug mode does), and propagating those leaks
// implementation detail / session state to callers.
//
// Headers in the allowlist below are case-insensitive matches against
// `http.CanonicalHeaderKey`. Operators who need additional headers
// from a specific backend should extend this list explicitly rather
// than disable the filter wholesale (C-30 from REVIEW-FAANG.md).
// NOTE: keys MUST be in Go's canonical form (`http.CanonicalHeaderKey`),
// not the RFC form. The stdlib canonicaliser preserves dash-separated
// camelcase but flattens runs of uppercase — e.g. RFC "ETag" → Go
// canonical "Etag"; RFC "X-Request-ID" → "X-Request-Id". The set
// below is keyed accordingly so the lookup in copyResponseHeaders
// matches.
var outboundResponseHeaderAllowlist = map[string]struct{}{
	"Content-Type":     {},
	"Content-Length":   {},
	"Content-Encoding": {},
	"Content-Language": {},
	"Etag":             {}, // Go-canonical form of "ETag"
	"Last-Modified":    {},
	"Cache-Control":    {},
	"Expires":          {},
	"Vary":             {},
	"X-Request-Id":     {}, // proxy-stamped; backends may echo it.
}

// alwaysDroppedHeaders is the belt-and-braces deny list — even when
// an operator extends the allowlist, these stay stripped because
// they carry session-binding semantics that MUST NOT cross the
// proxy boundary.
var alwaysDroppedHeaders = map[string]struct{}{
	"Set-Cookie":          {},
	"Set-Cookie2":         {},
	"Server":              {},
	"X-Powered-By":        {},
	"X-Aspnet-Version":    {},
	"X-Aspnetmvc-Version": {},
}

// copyResponseHeaders mirrors the upstream `resp.Headers` onto the
// inbound `w` after applying the allowlist + deny list. The two-pass
// scan is intentional: allowlist is the positive filter (drop
// anything not listed); deny list is the negative override (drop
// even if listed). Mirrors the inbound `compatutil.AllowlistedHeaders`
// shape so request + response sides have symmetric semantics.
func copyResponseHeaders(dst http.Header, src http.Header) {
	for k, vs := range src {
		canonical := http.CanonicalHeaderKey(k)
		if _, deny := alwaysDroppedHeaders[canonical]; deny {
			continue
		}
		if _, allow := outboundResponseHeaderAllowlist[canonical]; !allow {
			continue
		}
		for _, v := range vs {
			// One last belt-and-braces: drop CR/LF in header values
			// so a compromised backend cannot inject a fake
			// Set-Cookie via a Content-Type value.
			if strings.ContainsAny(v, "\r\n") {
				continue
			}
			dst.Add(canonical, v)
		}
	}
}
