package handlers

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

func defaultIsExternalURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme %q not allowed", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return errors.New("empty host")
	}
	// Resolve and reject if any address falls in a blocked range.
	addrs, err := net.LookupIP(host)
	if err != nil {
		// DNS failures are treated as untrusted (deny by default).
		return fmt.Errorf("dns lookup: %w", err)
	}
	for _, a := range addrs {
		if isBlockedIP(a) {
			return fmt.Errorf("address %s in blocked range", a.String())
		}
	}
	return nil
}

var blockedCIDRs = mustCIDRs([]string{
	"127.0.0.0/8",
	"10.0.0.0/8",
	"172.16.0.0/12",
	"192.168.0.0/16",
	"169.254.0.0/16", // link-local + cloud metadata 169.254.169.254
	"fd00::/8",       // ULA
	"fe80::/10",      // link-local v6
	"::1/128",
})

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

func isBlockedIP(ip net.IP) bool {
	if ip.IsUnspecified() {
		return true
	}
	for _, n := range blockedCIDRs {
		if n.Contains(ip) {
			return true
		}
	}
	// Sanity: also reject obviously suspicious user-supplied strings.
	if strings.Contains(ip.String(), "169.254.169.254") {
		return true
	}
	return false
}
