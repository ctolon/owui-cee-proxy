package handlers

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ResolvedURL is the result of resolving a user-supplied URL through
// the SSRF policy. The caller MUST use Dialer() to pin the eventual
// outbound connection to the resolved IP — otherwise a DNS-rebinding
// attacker can make the validation TOCTOU-vulnerable (B1).
type ResolvedURL struct {
	URL  *url.URL // parsed inbound URL
	Host string   // hostname (no port)
	Port string   // port string ("80", "443", ...)
	IP   net.IP   // single IP we validated; pin all dials here
}

// defaultResolveExternalURL parses raw, applies the SSRF policy
// (scheme, port allowlist, blocked CIDRs), resolves DNS once, and
// returns a ResolvedURL pinned to the chosen address.
//
// DNS-rebinding mitigation: we record the IP that PASSED validation
// and the caller is expected to dial that exact IP via Dialer().
// A second LookupIP at fetch time would otherwise be free to return
// 169.254.169.254 even though the first lookup returned a public IP.
func defaultResolveExternalURL(raw string) (*ResolvedURL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("scheme %q not allowed", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return nil, errors.New("empty host")
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "http" {
			port = "80"
		} else {
			port = "443"
		}
	}
	if !isAllowedPort(port) {
		return nil, fmt.Errorf("port %s not allowed", port)
	}

	// 5s ceiling on DNS resolution. Use the resolver's ctx-aware lookup
	// so the call honours cancellation (and so the noctx linter passes).
	dnsCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ipAddrs, err := net.DefaultResolver.LookupIPAddr(dnsCtx, host)
	if err != nil {
		// DNS failures are treated as untrusted (deny by default).
		return nil, fmt.Errorf("dns lookup: %w", err)
	}
	addrs := make([]net.IP, len(ipAddrs))
	for i := range ipAddrs {
		addrs[i] = ipAddrs[i].IP
	}
	if len(addrs) == 0 {
		return nil, errors.New("dns lookup: no addresses")
	}
	// All resolved addresses must pass — refuse if any one is blocked.
	// Otherwise an attacker can return [public, private] and we might
	// pick the public one while the OS dials the private one.
	for _, a := range addrs {
		if isBlockedIP(a) {
			return nil, fmt.Errorf("address %s in blocked range", a.String())
		}
	}
	// Pin the FIRST resolved IP. Subsequent dials must use this exact
	// address regardless of what later DNS lookups return.
	return &ResolvedURL{
		URL:  u,
		Host: host,
		Port: port,
		IP:   addrs[0],
	}, nil
}

// Dialer returns a DialContext function that ignores the host portion
// of addr and dials only the resolved IP. This is the SSRF-safe way to
// hand the URL to net/http: set this on a one-shot *http.Transport's
// DialContext field. The returned dialer enforces the resolved port
// too — callers cannot redirect to a different port via the URL.
func (r *ResolvedURL) Dialer() func(ctx context.Context, network, addr string) (net.Conn, error) {
	pinned := net.JoinHostPort(r.IP.String(), r.Port)
	d := &net.Dialer{}
	return func(ctx context.Context, network, _ string) (net.Conn, error) {
		return d.DialContext(ctx, network, pinned)
	}
}

// blockedCIDRs is the list of network ranges we never dial into.
//
// IPv4-mapped IPv6 (::ffff:a.b.c.d) is handled by stdlib `IsPrivate`/
// `IsLoopback` etc. in isBlockedIP — those internally call ip.To4() and
// match against the v4 CIDRs we already list. Adding `::ffff:0:0/96`
// here is tempting but a Go gotcha: its /96 mask combined with
// `networkNumberAndMask` collapses the network to 0.0.0.0/0 and ends
// up blocking the entire public IPv4 internet.
var blockedCIDRs = mustCIDRs([]string{
	"127.0.0.0/8",    // loopback
	"10.0.0.0/8",     // RFC1918
	"172.16.0.0/12",  // RFC1918
	"192.168.0.0/16", // RFC1918
	"169.254.0.0/16", // link-local + cloud metadata 169.254.169.254
	"0.0.0.0/8",      // "this network" (RFC1122)
	"100.64.0.0/10",  // CGNAT (RFC6598)
	"192.0.0.0/24",   // IETF protocol assignments
	"198.18.0.0/15",  // network benchmarking (RFC2544)
	"fd00::/8",       // ULA (IPv6 RFC1918 equivalent)
	"fe80::/10",      // link-local v6
	"::1/128",        // loopback v6
	"fd00:ec2::/64",  // AWS IMDSv2 IPv6 hint
})

// allowedPorts is the package-private port allowlist. Only standard
// HTTP(S) ports are accepted by default. Made a slice (not a set) so
// future config wiring can replace it without changing the lookup
// shape.
var allowedPorts = []string{"80", "443"}

func isAllowedPort(p string) bool {
	// Defensive: reject empty + non-numeric just to be safe.
	if p == "" {
		return false
	}
	if _, err := strconv.Atoi(p); err != nil {
		return false
	}
	for _, ap := range allowedPorts {
		if ap == p {
			return true
		}
	}
	return false
}

func mustCIDRs(items []string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(items))
	for _, s := range items {
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			panic("invalid blocked cidr: " + s)
		}
		out = append(out, n)
	}
	return out
}

// isBlockedIP returns true when ip falls in any blocked range OR when
// the standard library tags it as private/link-local/loopback/multicast/
// unspecified. We layer the stdlib check on top of the explicit CIDR
// list because IsPrivate() uses the IETF-maintained definition and
// catches edge cases (e.g., new RFC1918 additions) we might miss.
func isBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() ||
		ip.IsUnspecified() {
		return true
	}
	for _, n := range blockedCIDRs {
		if n.Contains(ip) {
			return true
		}
	}
	// Belt-and-braces: the cloud metadata endpoint should already be
	// caught by 169.254.0.0/16, but flag it explicitly so a future
	// edit can't silently remove the protection.
	if strings.Contains(ip.String(), "169.254.169.254") {
		return true
	}
	return false
}
