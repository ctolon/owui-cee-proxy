// Package reverseproxy provides a thin streaming reverse-proxy used by
// the native passthrough mounts (/docling/*, /tika/*, /kreuzberg/*).
//
// Hop-by-hop headers are stripped per RFC 7230. Authorization headers
// from the inbound request are dropped and replaced with the engine's
// own credentials (when configured) so callers cannot smuggle their
// own auth into the backend.
package reverseproxy

import (
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strings"
)

// New returns a *httputil.ReverseProxy that mounts the entire request
// onto baseURL after stripping prefix from the path. apiKeyHeader and
// apiKey, when non-empty, are injected on the outbound request.
//
// trustedProxies is a list of CIDR ranges whose inbound X-Forwarded-*
// (and Forwarded / X-Real-IP) headers may be preserved. Requests from
// IPs OUTSIDE this list have their forwarded-for headers stripped
// before SetXForwarded() runs, which prevents an untrusted client from
// spoofing the apparent source IP that reaches the backend (H3).
//
// An empty trustedProxies list means "trust nobody": all caller-set
// forwarded-for headers are stripped. This is the safe default.
func New(baseURL, prefix, apiKeyHeader, apiKey string, transport http.RoundTripper, trustedProxies []string) (*httputil.ReverseProxy, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}
	trusted, err := parseCIDRs(trustedProxies)
	if err != nil {
		return nil, err
	}
	rp := &httputil.ReverseProxy{
		Transport: transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			out := pr.Out
			out.URL.Scheme = u.Scheme
			out.URL.Host = u.Host
			out.Host = u.Host

			p := strings.TrimPrefix(pr.In.URL.Path, prefix)
			if !strings.HasPrefix(p, "/") {
				p = "/" + p
			}
			out.URL.Path = singleJoin(u.Path, p)
			out.URL.RawPath = ""

			cloned := pr.In.Header.Clone()
			if !isTrusted(pr.In.RemoteAddr, trusted) {
				// Untrusted source: drop any forwarded-for headers the
				// caller sent. SetXForwarded() below will repopulate
				// them from the actual RemoteAddr.
				dropForwardedHeaders(cloned)
			}
			out.Header = sanitizeHeaders(cloned)

			if apiKey != "" && apiKeyHeader != "" {
				out.Header.Set(apiKeyHeader, apiKey)
			}
			pr.SetXForwarded()
		},
	}
	return rp, nil
}

// RejectTraversal wraps next with a guard that 400s any request whose
// path contains "../" semantics after path.Clean. This is the M3 fix:
// path traversal would otherwise let an attacker reach engine endpoints
// (like /admin) outside the prefix the route mounts.
//
// Wired by routes.go in Wave 2; exposed here to keep all path-handling
// concerns in one package.
func RejectTraversal(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := r.URL.Path
		cleaned := path.Clean(raw)
		// path.Clean("/a/../b") -> "/b". Compare cleaned-vs-raw to
		// decide whether the request was attempting traversal. We
		// also reject any literal ".." segment to catch encoded
		// variants the muxer might have already decoded.
		if cleaned != raw || strings.Contains(raw, "..") {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func singleJoin(a, b string) string {
	a = strings.TrimRight(a, "/")
	if !strings.HasPrefix(b, "/") {
		b = "/" + b
	}
	// M3: path.Clean here is defence-in-depth — the RejectTraversal
	// wrapper SHOULD have caught a traversal attempt before we get
	// this far, but we also clean the joined path so a misconfigured
	// mount can never accidentally produce ".." in the URL we send
	// to the engine backend.
	return path.Clean(a + b)
}

// hopByHop is the set of hop-by-hop headers that must not be forwarded
// by an intermediary, per RFC 7230 §6.1.
var hopByHop = []string{
	"Connection",
	"Proxy-Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

func sanitizeHeaders(h http.Header) http.Header {
	for _, k := range hopByHop {
		h.Del(k)
	}
	// Drop caller-supplied Authorization so caller cannot inject auth
	// to the backend.
	h.Del("Authorization")
	return h
}

// dropForwardedHeaders removes the headers an untrusted caller could
// use to lie about their origin. After this runs, SetXForwarded() will
// set fresh values based on the actual RemoteAddr.
func dropForwardedHeaders(h http.Header) {
	h.Del("X-Forwarded-For")
	h.Del("X-Forwarded-Host")
	h.Del("X-Forwarded-Proto")
	h.Del("X-Forwarded-Port")
	h.Del("X-Real-Ip")
	h.Del("Forwarded")
}

// parseCIDRs converts a list of CIDR strings to *net.IPNet. An empty
// or nil input returns an empty slice (which means "trust nobody").
func parseCIDRs(items []string) ([]*net.IPNet, error) {
	out := make([]*net.IPNet, 0, len(items))
	for _, s := range items {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

// isTrusted reports whether remoteAddr ("ip:port") falls inside any of
// the trusted CIDRs. A malformed or empty address is treated as
// untrusted.
func isTrusted(remoteAddr string, trusted []*net.IPNet) bool {
	if len(trusted) == 0 {
		return false
	}
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, n := range trusted {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}
