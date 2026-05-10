package handlers

import (
	"context"
	"net"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestResolvedURL_DialerPinsResolvedIP exercises the DNS-rebinding TOCTOU
// scenario from B1: imagine the resolver returns a public IP at policy
// time and 169.254.169.254 at fetch time. The fix is that the caller
// uses ResolvedURL.Dialer() instead of redoing DNS, so the second
// "lookup" is bypassed entirely.
func TestResolvedURL_DialerPinsResolvedIP(t *testing.T) {
	t.Parallel()

	// Stand up a TCP listener on 127.0.0.1 and aim the dialer at the
	// resolved-once IP for the listener's port. Any address the caller
	// passes (e.g. "169.254.169.254:1") must be ignored.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	host, port, err := net.SplitHostPort(ln.Addr().String())
	require.NoError(t, err)

	r := &ResolvedURL{
		Host: "example.test",
		Port: port,
		IP:   net.ParseIP(host),
	}
	dial := r.Dialer()

	// Caller passes a "rebound" address that points at link-local.
	// The dialer must ignore the addr arg and connect to the listener.
	conn, err := dial(context.Background(), "tcp", "169.254.169.254:1")
	require.NoError(t, err, "dialer should ignore caller addr and use pinned IP")
	defer conn.Close()
	require.Equal(t, ln.Addr().String(), conn.RemoteAddr().String())
}

// TestIsBlockedIP_NewCIDRs covers the ranges added in B2: each must be
// rejected.
func TestIsBlockedIP_NewCIDRs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		ip   string
	}{
		{"this-network 0.0.0.0/8", "0.1.2.3"},
		{"cgnat 100.64.0.0/10", "100.64.1.1"},
		{"protocol-assignments 192.0.0.0/24", "192.0.0.5"},
		{"benchmarking 198.18.0.0/15", "198.18.0.1"},
		{"benchmarking-upper 198.19.255.254", "198.19.255.254"},
		{"ipv4-mapped-v6", "::ffff:10.0.0.1"},
		{"loopback v4", "127.0.0.1"},
		{"private 10.0.0.0/8", "10.1.2.3"},
		{"private 172.16.0.0/12", "172.16.5.5"},
		{"private 192.168.0.0/16", "192.168.1.1"},
		{"link-local 169.254.0.0/16", "169.254.169.254"},
		{"loopback v6", "::1"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ip := net.ParseIP(c.ip)
			require.NotNil(t, ip, "parse: %s", c.ip)
			require.True(t, isBlockedIP(ip), "%s should be blocked", c.ip)
		})
	}
}

// TestIsBlockedIP_AllowsPublic confirms we don't accidentally block all
// of the public Internet.
func TestIsBlockedIP_AllowsPublic(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"1.1.1.1", "8.8.8.8", "104.16.0.1", "2001:4860:4860::8888"} {
		ip := net.ParseIP(s)
		require.NotNil(t, ip)
		require.False(t, isBlockedIP(ip), "%s must not be blocked", s)
	}
}

// TestIsAllowedPort exercises the port allowlist (B2).
func TestIsAllowedPort(t *testing.T) {
	t.Parallel()
	cases := []struct {
		port string
		want bool
	}{
		{"80", true},
		{"443", true},
		{"22", false},
		{"6379", false},
		{"8080", false},
		{"", false},
		{"abc", false},
	}
	for _, c := range cases {
		c := c
		t.Run(c.port, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, c.want, isAllowedPort(c.port))
		})
	}
}

// TestDefaultResolveExternalURL_PortRejection covers the URL-level
// integration of the port allowlist.
func TestDefaultResolveExternalURL_PortRejection(t *testing.T) {
	t.Parallel()
	// 1.1.1.1 is public, but :22 must still be rejected for SSH.
	_, err := defaultResolveExternalURL("https://1.1.1.1:22/x")
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "port"), "want port error, got %v", err)

	// :443 on 1.1.1.1 is allowed by policy.
	rURL, err := defaultResolveExternalURL("https://1.1.1.1:443/x")
	require.NoError(t, err)
	require.Equal(t, "443", rURL.Port)
	require.NotNil(t, rURL.IP)
}

// TestDefaultResolveExternalURL_BlocksPrivate confirms the resolver
// rejects RFC1918 inputs even when the URL parses fine.
func TestDefaultResolveExternalURL_BlocksPrivate(t *testing.T) {
	t.Parallel()
	_, err := defaultResolveExternalURL("https://10.0.0.1/x")
	require.Error(t, err)
}
