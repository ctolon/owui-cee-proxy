package http

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

	"github.com/rs/zerolog"

	"github.com/ctolon/owui-cee-proxy/internal/config"
)

type Server struct {
	httpServer *http.Server
	listener   net.Listener
	tlsCfg     *tls.Config
	logger     zerolog.Logger
	cfg        config.ServerConfig
}

func NewServer(cfg config.ServerConfig, handler http.Handler, logger zerolog.Logger) (*Server, error) {
	tlsCfg, err := buildTLS(cfg.TLS)
	if err != nil {
		return nil, err
	}
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           handler,
		ReadTimeout:       cfg.ReadTimeout,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		MaxHeaderBytes:    cfg.MaxHeaderBytes,
		TLSConfig:         tlsCfg,
		ErrorLog: nil,
	}
	return &Server{httpServer: srv, tlsCfg: tlsCfg, logger: logger, cfg: cfg}, nil
}

func (s *Server) ListenAndServe() error {
	ln, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.cfg.Listen, err)
	}
	s.listener = ln
	s.logger.Info().Str("addr", s.cfg.Listen).Bool("tls", s.cfg.TLS.Enabled).Msg("server_listening")

	if s.cfg.TLS.Enabled {
		return s.httpServer.ServeTLS(ln, s.cfg.TLS.CertFile, s.cfg.TLS.KeyFile)
	}
	return s.httpServer.Serve(ln)
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}

func buildTLS(cfg config.TLSServerConfig) (*tls.Config, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	tc := &tls.Config{
		MinVersion: tls.VersionTLS13,
	}
	if cfg.MinVersion == "1.2" {
		tc.MinVersion = tls.VersionTLS12
	}
	switch cfg.ClientAuth {
	case "", "none":
		tc.ClientAuth = tls.NoClientCert
	case "request":
		tc.ClientAuth = tls.RequestClientCert
	case "require":
		tc.ClientAuth = tls.RequireAnyClientCert
	case "verify":
		tc.ClientAuth = tls.VerifyClientCertIfGiven
	case "require-and-verify":
		tc.ClientAuth = tls.RequireAndVerifyClientCert
	default:
		return nil, fmt.Errorf("unknown client_auth %q", cfg.ClientAuth)
	}
	if cfg.ClientCAFile != "" {
		pem, err := os.ReadFile(cfg.ClientCAFile) //nolint:gosec
		if err != nil {
			return nil, fmt.Errorf("read client CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("invalid client CA pem")
		}
		tc.ClientCAs = pool
	}
	return tc, nil
}

// shutdownTimeout is exported for the composition root to align with config.
func (s *Server) GracePeriod() time.Duration {
	if s.cfg.ShutdownGrace <= 0 {
		return 30 * time.Second
	}
	return s.cfg.ShutdownGrace
}
