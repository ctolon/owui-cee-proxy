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

	"github.com/ctolon/owui-cee-proxy/internal/config"
)

// Client wraps an *http.Client plus the request timeout configured for
// the backend.
type Client struct {
	HTTP           *http.Client
	RequestTimeout time.Duration
	BaseURL        string
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
	requestTimeout := orDuration(cfg.RequestTimeout, 120*time.Second)
	responseHeaderTimeout := cfg.HTTP.ResponseHeaderTimeout
	if responseHeaderTimeout <= 0 {
		responseHeaderTimeout = requestTimeout
	}

	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          orDefault(cfg.HTTP.MaxIdleConns, 256),
		MaxIdleConnsPerHost:   orDefault(cfg.HTTP.MaxIdleConnsPerHost, 64),
		MaxConnsPerHost:       cfg.HTTP.MaxConnsPerHost,
		IdleConnTimeout:       orDuration(cfg.HTTP.IdleConnTimeout, 90*time.Second),
		TLSHandshakeTimeout:   orDuration(cfg.HTTP.TLSHandshakeTimeout, 10*time.Second),
		ResponseHeaderTimeout: responseHeaderTimeout,
		ExpectContinueTimeout: orDuration(cfg.HTTP.ExpectContinueTimeout, time.Second),
		DisableCompression:    cfg.HTTP.DisableCompression,
		TLSClientConfig:       tlsConf,
	}
	return &Client{
		HTTP: &http.Client{
			Transport: transport,
			Timeout:   0, // we use per-request context deadlines instead
		},
		RequestTimeout: requestTimeout,
		BaseURL:        cfg.URL,
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
