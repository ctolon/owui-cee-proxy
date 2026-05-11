package handlers

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
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

// SSRFRejectReason names the taxonomy the metrics layer surfaces in
// `ssrf_rejected_total{reason}`. Naming is stable across releases so
// dashboards built on these strings keep working; ADD new reasons,
// never rename. Reason taxonomy doubles as the warn-log `reason`
// field so log/metric grouping aligns.
type SSRFRejectReason string

const (
	SSRFReasonInvalidURL       SSRFRejectReason = "invalid_url"
	SSRFReasonNonHTTPScheme    SSRFRejectReason = "non_http_scheme"
	SSRFReasonEmptyHost        SSRFRejectReason = "empty_host"
	SSRFReasonPortNotAllowed   SSRFRejectReason = "port_not_allowed"
	SSRFReasonResolutionFailed SSRFRejectReason = "resolution_failed"
	SSRFReasonResolutionEmpty  SSRFRejectReason = "resolution_empty"
	SSRFReasonLoopback         SSRFRejectReason = "loopback"
	SSRFReasonLinkLocal        SSRFRejectReason = "link_local"
	SSRFReasonMetadataIP       SSRFRejectReason = "metadata_ip"
	SSRFReasonMulticast        SSRFRejectReason = "multicast"
	SSRFReasonPrivate          SSRFRejectReason = "private"
	SSRFReasonBlockedCIDR      SSRFRejectReason = "blocked_cidr"
	SSRFReasonUnspecified      SSRFRejectReason = "unspecified"
)

// SSRFMetrics is the optional observability seam for the resolver.
// Nil-safe: a Resolver constructed without metrics simply skips the
// callbacks. Production wiring binds RecordRejection to the
// SSRFRejected counter and RecordCache* to SSRFDNSCache.
type SSRFMetrics interface {
	RecordRejection(reason SSRFRejectReason)
	RecordDNSCacheHit()
	RecordDNSCacheMiss()
	RecordDNSCacheBypass()
}

// nopSSRFMetrics is the safe default when no observability layer is
// wired (unit tests, contract suites).
type nopSSRFMetrics struct{}

func (nopSSRFMetrics) RecordRejection(SSRFRejectReason) {}
func (nopSSRFMetrics) RecordDNSCacheHit()               {}
func (nopSSRFMetrics) RecordDNSCacheMiss()              {}
func (nopSSRFMetrics) RecordDNSCacheBypass()            {}

// SSRFLogger is the structured log sink for rejection events. nil
// means "don't log". A typical wiring uses the same proxy zerolog
// instance the rest of the handlers do.
type SSRFLogger interface {
	LogRejection(host, raw string, reason SSRFRejectReason, err error)
}

type noopSSRFLogger struct{}

func (noopSSRFLogger) LogRejection(string, string, SSRFRejectReason, error) {}

// Resolver applies the SSRF policy with optional metric + log
// callbacks and an in-memory DNS cache. Concurrency-safe (cache
// mutates under its own lock; metrics/logger are required to be
// goroutine-safe themselves). A zero-value Resolver acts as the
// historical defaultResolveExternalURL — no metrics, no cache.
type Resolver struct {
	Metrics  SSRFMetrics
	Logger   SSRFLogger
	cache    *dnsCache
	sf       singleflight.Group
	dnsClock func() time.Time // injectable for tests
	resolver func(ctx context.Context, host string) ([]net.IPAddr, error)
}

// NewResolver builds a Resolver with the supplied DNS cache TTL.
// ttl<=0 disables the cache (every lookup hits net.DefaultResolver).
// MaxEntries default 256 prevents unbounded growth under URL fuzzing.
func NewResolver(ttl time.Duration, maxEntries int, m SSRFMetrics, log SSRFLogger) *Resolver {
	if m == nil {
		m = nopSSRFMetrics{}
	}
	if log == nil {
		log = noopSSRFLogger{}
	}
	r := &Resolver{
		Metrics:  m,
		Logger:   log,
		dnsClock: time.Now,
		resolver: func(ctx context.Context, host string) ([]net.IPAddr, error) {
			return net.DefaultResolver.LookupIPAddr(ctx, host)
		},
	}
	if ttl > 0 {
		if maxEntries <= 0 {
			maxEntries = 256
		}
		r.cache = newDNSCache(ttl, maxEntries)
	}
	return r
}

// Resolve parses raw, applies the SSRF policy, and returns a
// ResolvedURL pinned to a vetted IP. Reasons for rejection are
// reported to the metrics + logger callbacks so operators can alert
// on `metadata_ip>0` (active credential-exfil probing).
func (r *Resolver) Resolve(raw string) (*ResolvedURL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		r.reject("", raw, SSRFReasonInvalidURL, err)
		return nil, fmt.Errorf("invalid url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		r.reject(u.Host, raw, SSRFReasonNonHTTPScheme, nil)
		return nil, fmt.Errorf("scheme %q not allowed", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		r.reject("", raw, SSRFReasonEmptyHost, nil)
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
		r.reject(host, raw, SSRFReasonPortNotAllowed, nil)
		return nil, fmt.Errorf("port %s not allowed", port)
	}

	addrs, err := r.lookup(host)
	if err != nil {
		r.reject(host, raw, SSRFReasonResolutionFailed, err)
		return nil, fmt.Errorf("dns lookup: %w", err)
	}
	if len(addrs) == 0 {
		r.reject(host, raw, SSRFReasonResolutionEmpty, nil)
		return nil, errors.New("dns lookup: no addresses")
	}
	for _, a := range addrs {
		reason := classifyIP(a)
		if reason != "" {
			r.reject(host, raw, reason, nil)
			return nil, fmt.Errorf("address %s in blocked range", a.String())
		}
	}
	return &ResolvedURL{URL: u, Host: host, Port: port, IP: addrs[0]}, nil
}

// lookup performs the DNS resolution, going through the LRU cache +
// singleflight dedup when configured. A bypass (cache disabled) path
// emits a `bypass` cache event so the operator can tell apart "no
// cache" from "cache miss".
func (r *Resolver) lookup(host string) ([]net.IP, error) {
	if r.cache == nil {
		r.Metrics.RecordDNSCacheBypass()
		return r.directLookup(host)
	}
	if ips, ok := r.cache.get(host, r.dnsClock()); ok {
		r.Metrics.RecordDNSCacheHit()
		return ips, nil
	}
	v, err, _ := r.sf.Do(host, func() (interface{}, error) {
		// Double-check after winning the singleflight race in case
		// another caller populated the cache while we were queued.
		if ips, ok := r.cache.get(host, r.dnsClock()); ok {
			r.Metrics.RecordDNSCacheHit()
			return ips, nil
		}
		ips, err := r.directLookup(host)
		if err != nil {
			return nil, err
		}
		r.cache.put(host, ips, r.dnsClock())
		r.Metrics.RecordDNSCacheMiss()
		return ips, nil
	})
	if err != nil {
		return nil, err
	}
	return v.([]net.IP), nil
}

// directLookup is the underlying DNS call with a 5s ceiling.
func (r *Resolver) directLookup(host string) ([]net.IP, error) {
	dnsCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ipAddrs, err := r.resolver(dnsCtx, host)
	if err != nil {
		return nil, err
	}
	out := make([]net.IP, len(ipAddrs))
	for i := range ipAddrs {
		out[i] = ipAddrs[i].IP
	}
	return out, nil
}

func (r *Resolver) reject(host, raw string, reason SSRFRejectReason, err error) {
	r.Metrics.RecordRejection(reason)
	r.Logger.LogRejection(host, raw, reason, err)
}

// defaultResolver is the zero-config zero-cache resolver retained
// for the package-private default path (tests that don't construct
// a full Resolver still get correct policy enforcement).
var defaultResolver = NewResolver(0, 0, nil, nil)

// defaultResolveExternalURL keeps the historical free-function
// signature alive for callers that haven't been migrated to the
// Resolver struct yet. New code should construct a Resolver and call
// Resolve directly to benefit from caching + observability.
func defaultResolveExternalURL(raw string) (*ResolvedURL, error) {
	return defaultResolver.Resolve(raw)
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
// `IsLoopback` etc. in classifyIP — those internally call ip.To4()
// and match against the v4 CIDRs we already list. Adding `::ffff:0:0/96`
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

// mustCIDRs parses an in-package CIDR literal list. Since the input
// is a code-controlled constant, parsing failure is a programming
// error worth panicking on at init time (NOT a config-driven runtime
// failure — operator-supplied CIDRs go through config.Validate's
// validation in Validate, never through this function).
func mustCIDRs(items []string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(items))
	for _, s := range items {
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			panic("internal: invalid blocked cidr literal: " + s)
		}
		out = append(out, n)
	}
	return out
}

// isBlockedIP is a thin boolean adapter over classifyIP, retained
// for the existing test suite. New code should prefer classifyIP
// since it carries the reason taxonomy needed by metrics + logging.
func isBlockedIP(ip net.IP) bool { return classifyIP(ip) != "" }

// classifyIP returns the rejection reason if the IP is not safe to
// dial, or "" when the IP is acceptable. Splitting the classification
// from the boolean check lets the metrics layer label rejections by
// the specific policy that fired, instead of the catch-all
// "blocked_cidr".
func classifyIP(ip net.IP) SSRFRejectReason {
	if ip == nil {
		return SSRFReasonUnspecified
	}
	switch {
	case ip.IsUnspecified():
		return SSRFReasonUnspecified
	case ip.IsLoopback():
		return SSRFReasonLoopback
	case ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast():
		// 169.254.169.254 is the cloud-metadata IP — flag distinctly
		// for alerting purposes (active SSRF probing).
		if strings.Contains(ip.String(), "169.254.169.254") {
			return SSRFReasonMetadataIP
		}
		return SSRFReasonLinkLocal
	case ip.IsMulticast():
		return SSRFReasonMulticast
	case ip.IsPrivate():
		return SSRFReasonPrivate
	}
	for _, n := range blockedCIDRs {
		if n.Contains(ip) {
			// Even though IsPrivate/IsLoopback caught most ranges, the
			// CIDR list adds CGNAT, RFC2544 benchmarking, IETF
			// assignments, etc. Catch-all bucket — alerts on this
			// indicate an exotic destination, not the common ones.
			return SSRFReasonBlockedCIDR
		}
	}
	return ""
}

// dnsCache is a tiny TTL cache. Not LRU strictly; on overflow it
// evicts the oldest entry by lastInserted timestamp. Adequate at the
// max-256 scale we target — overhead of a true LRU isn't worth it.
type dnsCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	max     int
	entries map[string]dnsCacheEntry
}

type dnsCacheEntry struct {
	ips       []net.IP
	expiresAt time.Time
}

func newDNSCache(ttl time.Duration, max int) *dnsCache {
	return &dnsCache{
		ttl:     ttl,
		max:     max,
		entries: make(map[string]dnsCacheEntry, max),
	}
}

func (c *dnsCache) get(host string, now time.Time) ([]net.IP, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[host]
	if !ok || e.expiresAt.Before(now) {
		return nil, false
	}
	return e.ips, true
}

func (c *dnsCache) put(host string, ips []net.IP, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= c.max {
		// Evict the entry closest to expiry — cheap proxy for LRU.
		var oldestKey string
		var oldestExpiry time.Time
		for k, v := range c.entries {
			if oldestKey == "" || v.expiresAt.Before(oldestExpiry) {
				oldestKey = k
				oldestExpiry = v.expiresAt
			}
		}
		delete(c.entries, oldestKey)
	}
	c.entries[host] = dnsCacheEntry{
		ips:       ips,
		expiresAt: now.Add(c.ttl),
	}
}
