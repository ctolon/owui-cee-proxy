// Package httpclient builds tuned http.Clients for talking to backend
// CEE engines. Each backend gets its own Client so connection pools and
// timeouts cannot bleed across engines.
package httpclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/propagation"
	"golang.org/x/time/rate"

	"github.com/ctolon/owui-cee-proxy/internal/config"
	"github.com/ctolon/owui-cee-proxy/internal/engine/authutil"
)

// Tunables exported for operator-facing config defaults. Keep these
// values literal — the loader documentation references them by name
// so a change here must be paired with the docs/CHANGELOG.
const (
	// DefaultMaxIdleConns is the per-engine HTTP client's connection
	// pool ceiling. 256 is generous for ≤16 backends; bump for
	// fan-out-heavy deployments via engine.http.max_idle_conns.
	DefaultMaxIdleConns = 256
	// DefaultMaxIdleConnsPerHost limits idle keep-alive to one host
	// (engines are usually single-host). Higher values dilute the
	// keep-alive benefit; lower stalls bursty traffic.
	DefaultMaxIdleConnsPerHost = 64
	// DefaultRequestTimeout caps each engine.Convert call.
	DefaultRequestTimeout = 120 * time.Second
	// DefaultDialTimeout is the TCP dial ceiling.
	DefaultDialTimeout = 10 * time.Second
	// DefaultDialKeepAlive sets the SO_KEEPALIVE period on the
	// dialer; aligned with the typical kube-proxy idle timeout.
	DefaultDialKeepAlive = 30 * time.Second
	// DefaultIdleConnTimeout reaps idle connections.
	DefaultIdleConnTimeout = 90 * time.Second
	// DefaultTLSHandshakeTimeout caps the TLS handshake stage.
	DefaultTLSHandshakeTimeout = 10 * time.Second
	// DefaultExpectContinueTimeout is the Go default; named so config
	// readers can find it.
	DefaultExpectContinueTimeout = 1 * time.Second
)

// Client wraps an *http.Client plus the request timeout configured for
// the backend.
type Client struct {
	HTTP           *http.Client
	RequestTimeout time.Duration
	BaseURL        string
	// innerTransport is the raw *http.Transport before any
	// otelhttp.NewTransport / instrumented wrap. Tests + introspection
	// helpers reach the underlying knobs (MaxIdleConns, dialer,
	// ResponseHeaderTimeout) through `InnerTransport()` rather than
	// asserting against c.HTTP.Transport's concrete type, which would
	// break the moment another wrapper layer is added.
	innerTransport *http.Transport
	// authMetrics is the seam adapters use to report auth-stamping
	// outcomes to the metrics layer. Nil-safe: callers MUST go through
	// Auth() rather than reading the field directly so they always
	// get a usable AuthMetrics (Nop when nothing was injected).
	authMetrics authutil.AuthMetrics
}

// InnerTransport returns the raw *http.Transport without OTel or
// instrumented wrapper layers. Test-only API; production code paths
// dial through c.HTTP.Transport which is the wrapped form.
func (c *Client) InnerTransport() *http.Transport { return c.innerTransport }

// Auth returns the configured AuthMetrics hook, falling back to a Nop
// implementation when none was injected. Adapters call this on every
// outbound request so unit tests (which don't bother wiring metrics)
// don't need to know about the seam at all.
func (c *Client) Auth() authutil.AuthMetrics {
	if c.authMetrics == nil {
		return authutil.Nop{}
	}
	return c.authMetrics
}

// WithAuthMetrics wires a metrics callback for ApplyAuth decisions and
// returns the same Client so the call composes after New() in the
// composition root: `httpclient.New(cfg).WithAuthMetrics(m)`. Idempotent
// — passing nil clears the hook (back to Nop).
func (c *Client) WithAuthMetrics(m authutil.AuthMetrics) *Client {
	c.authMetrics = m
	return c
}

// New constructs a Client from the per-engine configuration.
func New(cfg config.EngineConfig) (*Client, error) {
	tlsConf, err := buildTLS(cfg.HTTP.TLS)
	if err != nil {
		return nil, err
	}
	// M7: ResponseHeaderTimeout previously hard-defaulted to 30s,
	// which trips on legitimate large-PDF jobs that take longer than
	// that to emit response headers (e.g. Docling running OCR before
	// it returns the JSON envelope). Default to RequestTimeout so the
	// transport-level header timeout aligns with the per-engine
	// budget; operators who actually want a tighter header timeout
	// can still set http.response_header_timeout explicitly.
	requestTimeout := orDuration(cfg.RequestTimeout, DefaultRequestTimeout)
	responseHeaderTimeout := cfg.HTTP.ResponseHeaderTimeout
	if responseHeaderTimeout <= 0 {
		responseHeaderTimeout = requestTimeout
	}

	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   DefaultDialTimeout,
			KeepAlive: DefaultDialKeepAlive,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          orDefault(cfg.HTTP.MaxIdleConns, DefaultMaxIdleConns),
		MaxIdleConnsPerHost:   orDefault(cfg.HTTP.MaxIdleConnsPerHost, DefaultMaxIdleConnsPerHost),
		MaxConnsPerHost:       cfg.HTTP.MaxConnsPerHost,
		IdleConnTimeout:       orDuration(cfg.HTTP.IdleConnTimeout, DefaultIdleConnTimeout),
		TLSHandshakeTimeout:   orDuration(cfg.HTTP.TLSHandshakeTimeout, DefaultTLSHandshakeTimeout),
		ResponseHeaderTimeout: responseHeaderTimeout,
		ExpectContinueTimeout: orDuration(cfg.HTTP.ExpectContinueTimeout, DefaultExpectContinueTimeout),
		DisableCompression:    cfg.HTTP.DisableCompression,
		TLSClientConfig:       tlsConf,
	}
	// Wrap the per-engine transport in otelhttp.NewTransport so
	// outbound requests carry W3C `traceparent` + `tracestate` +
	// `baggage` headers — the engine backend then continues the
	// trace started by the proxy's inbound otelhttp middleware,
	// producing one end-to-end span tree in Tempo / Jaeger. The
	// hand-rolled InstrumentedTransport stays downstream of this
	// wrap (added later via Instrument()) so existing Prometheus
	// counters continue to fire; OTel just adds the propagation
	// layer.
	//
	// Pass the propagator explicitly rather than reading
	// otel.GetTextMapPropagator() so this wrap fires correctly even
	// when SetupTracing has not run (unit tests, dev cycles with
	// observability disabled). production tracing.go ALSO sets the
	// global to the same composite; tests need not.
	// (C-19, P1 observability review.)
	var underlying http.RoundTripper = transport
	// Per-engine rate limit (C-24). The YAML `engines.<n>.rate_limit`
	// knob now drives a token-bucket limiter that gates every
	// outbound request. RPS == 0 disables — operators who never
	// configured rate_limit (or set both fields to 0) see no
	// behaviour change. Sits BELOW the otelhttp + Instrumented
	// wraps so the limiter's wait time is captured in the trace
	// span and the metric duration.
	if cfg.RateLimit.RPS > 0 {
		burst := cfg.RateLimit.Burst
		if burst <= 0 {
			burst = 1
		}
		underlying = &RateLimitedTransport{
			Limiter: rate.NewLimiter(rate.Limit(cfg.RateLimit.RPS), burst),
			Next:    underlying,
		}
	}
	rt := otelhttp.NewTransport(
		underlying,
		otelhttp.WithPropagators(propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{}, propagation.Baggage{},
		)),
	)
	return &Client{
		HTTP: &http.Client{
			Transport: rt,
			Timeout:   0, // we use per-request context deadlines instead
		},
		RequestTimeout: requestTimeout,
		BaseURL:        cfg.URL,
		innerTransport: transport,
	}, nil
}

// WithDeadline returns a context that combines the caller deadline with
// the engine's configured request timeout (whichever is sooner).
func (c *Client) WithDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	if c.RequestTimeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, c.RequestTimeout)
}

func buildTLS(c config.TLSClientConfig) (*tls.Config, error) {
	tc := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: c.InsecureSkipVerify, // #nosec G402 -- explicit opt-in via config; documented as DANGEROUS
		ServerName:         c.ServerName,
	}
	if c.CAFile != "" {
		pool, err := loadCAPool(c.CAFile)
		if err != nil {
			return nil, err
		}
		tc.RootCAs = pool
	}
	if c.CertFile != "" || c.KeyFile != "" {
		if c.CertFile == "" || c.KeyFile == "" {
			return nil, errors.New("httpclient: TLS cert_file and key_file must both be set")
		}
		cert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("httpclient: load client cert: %w", err)
		}
		tc.Certificates = []tls.Certificate{cert}
	}
	return tc, nil
}

func loadCAPool(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path) // #nosec G304 -- path is operator-supplied trust bundle path from validated config
	if err != nil {
		return nil, fmt.Errorf("httpclient: read CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("httpclient: no PEM data in CA file")
	}
	return pool, nil
}

func orDefault(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

func orDuration(v, def time.Duration) time.Duration {
	if v <= 0 {
		return def
	}
	return v
}
