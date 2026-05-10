// Package reverseproxy provides a thin streaming reverse-proxy used by
// the native passthrough mounts (/docling/*, /tika/*, /kreuzberg/*).
//
// Hop-by-hop headers are stripped per RFC 7230. Authorization headers
// from the inbound request are dropped and replaced with the engine's
// own credentials (when configured) so callers cannot smuggle their
// own auth into the backend.
package reverseproxy

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// New returns a *httputil.ReverseProxy that mounts the entire request
// onto baseURL after stripping prefix from the path. apiKeyHeader and
// apiKey, when non-empty, are injected on the outbound request.
func New(baseURL, prefix, apiKeyHeader, apiKey string, transport http.RoundTripper) (*httputil.ReverseProxy, error) {
	u, err := url.Parse(baseURL)
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

			path := strings.TrimPrefix(pr.In.URL.Path, prefix)
			if !strings.HasPrefix(path, "/") {
				path = "/" + path
			}
			out.URL.Path = singleJoin(u.Path, path)
			out.URL.RawPath = ""

			out.Header = sanitizeHeaders(pr.In.Header.Clone())

			if apiKey != "" && apiKeyHeader != "" {
				out.Header.Set(apiKeyHeader, apiKey)
			}
			pr.SetXForwarded()
		},
	}
	return rp, nil
}

func singleJoin(a, b string) string {
	a = strings.TrimRight(a, "/")
	if !strings.HasPrefix(b, "/") {
		b = "/" + b
	}
	return a + b
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
