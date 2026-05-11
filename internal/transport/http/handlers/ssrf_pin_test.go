package handlers

import (
	"errors"
	"net"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ctolon/owui-cee-proxy/internal/engine"
)

// TestValidateSources_PinsURLToResolvedIP pins C-44: after a
// successful SSRF validation, the source URL is rewritten to use
// the resolved IP literal and the original hostname goes into the
// Host header. The engine backend dialing this rewritten URL
// cannot fall through to a fresh DNS lookup (the attacker's
// rebinding window is closed).
func TestValidateSources_PinsURLToResolvedIP(t *testing.T) {
	t.Parallel()
	srcs := []engine.HTTPSource{
		{URL: "https://example.com/report.pdf"},
	}
	c := &Convert{
		ResolveURL: func(raw string) (*ResolvedURL, error) {
			u, _ := url.Parse(raw)
			return &ResolvedURL{
				URL:  u,
				Host: u.Hostname(),
				Port: "443",
				IP:   net.IPv4(93, 184, 216, 34), // example.com's classic IP
			}, nil
		},
	}

	require.NoError(t, c.validateSources(srcs))
	require.Equal(t, "https://93.184.216.34:443/report.pdf", srcs[0].URL,
		"source URL MUST be rewritten to the resolved IP literal")
	require.Equal(t, "example.com", srcs[0].Headers["Host"],
		"original hostname MUST be preserved in the Host header for vhost / SNI")
}

// TestValidateSources_PinsIPv6Bracketed exercises the IPv6 path:
// the literal MUST be wrapped in brackets so the URL parser
// recognises it as a host with port.
func TestValidateSources_PinsIPv6Bracketed(t *testing.T) {
	t.Parallel()
	srcs := []engine.HTTPSource{
		{URL: "https://example.com/doc"},
	}
	c := &Convert{
		ResolveURL: func(raw string) (*ResolvedURL, error) {
			u, _ := url.Parse(raw)
			return &ResolvedURL{
				URL:  u,
				Host: u.Hostname(),
				Port: "443",
				IP:   net.ParseIP("2606:2800:220:1:248:1893:25c8:1946"),
			}, nil
		},
	}
	require.NoError(t, c.validateSources(srcs))
	require.Contains(t, srcs[0].URL, "[2606:2800:220:1:248:1893:25c8:1946]:443",
		"IPv6 literal MUST be bracketed")
}

// TestValidateSources_PreservesCallerSuppliedHost — if the caller
// explicitly sent a Host header, we don't clobber it. Operators
// driving a CDN front have specific Host overrides.
func TestValidateSources_PreservesCallerSuppliedHost(t *testing.T) {
	t.Parallel()
	srcs := []engine.HTTPSource{
		{URL: "https://example.com/doc", Headers: map[string]string{"Host": "cdn.example.com"}},
	}
	c := &Convert{
		ResolveURL: func(raw string) (*ResolvedURL, error) {
			u, _ := url.Parse(raw)
			return &ResolvedURL{
				URL: u, Host: u.Hostname(), Port: "443",
				IP: net.IPv4(1, 2, 3, 4),
			}, nil
		},
	}
	require.NoError(t, c.validateSources(srcs))
	require.Equal(t, "cdn.example.com", srcs[0].Headers["Host"],
		"caller-supplied Host MUST win over our default")
}

// TestValidateSources_RejectionPropagates — the rewriting only
// applies on success. A rejected URL surfaces the error verbatim.
func TestValidateSources_RejectionPropagates(t *testing.T) {
	t.Parallel()
	srcs := []engine.HTTPSource{
		{URL: "http://169.254.169.254/latest/meta-data/"},
	}
	c := &Convert{
		ResolveURL: func(_ string) (*ResolvedURL, error) {
			return nil, errors.New("blocked: metadata_ip")
		},
	}
	err := c.validateSources(srcs)
	require.Error(t, err)
	require.Contains(t, err.Error(), "169.254.169.254")
}
