// Package app is the composition root. It wires the configuration into
// concrete adapter implementations and returns a runnable Application.
// The HTTP transport, async worker, and observability subsystems all
// receive their dependencies through this single seam.
package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/rs/zerolog"

	"github.com/ctolon/owui-cee-proxy/internal/breaker"
	"github.com/ctolon/owui-cee-proxy/internal/config"
	"github.com/ctolon/owui-cee-proxy/internal/engine"
	"github.com/ctolon/owui-cee-proxy/internal/engine/compat/docling"
	"github.com/ctolon/owui-cee-proxy/internal/engine/compat/doclingexternal"
	"github.com/ctolon/owui-cee-proxy/internal/engine/compat/external"
	"github.com/ctolon/owui-cee-proxy/internal/engine/compat/tika"
	"github.com/ctolon/owui-cee-proxy/internal/httpclient"
	"github.com/ctolon/owui-cee-proxy/internal/observability"
	"github.com/ctolon/owui-cee-proxy/internal/tasks"
	httptransport "github.com/ctolon/owui-cee-proxy/internal/transport/http"
)

type Application struct {
	cfg           *config.Config
	logger        zerolog.Logger
	server        *httptransport.Server
	worker        *tasks.Worker
	orchestrator  *tasks.Orchestrator
	traceShutdown func(context.Context) error
}

func Build(ctx context.Context, cfg *config.Config) (*Application, error) {
	logger := observability.NewLogger(cfg.Observability.Log)
	traceShutdown, err := observability.SetupTracing(ctx, cfg.Observability.Tracing)
	if err != nil {
		return nil, fmt.Errorf("tracing: %w", err)
	}

	registry, err := buildRegistry(cfg)
	if err != nil {
		return nil, err
	}

	metrics := observability.NewMetrics()

	var orch *tasks.Orchestrator
	var worker *tasks.Worker
	var redisHealth func(context.Context) error
	if cfg.Tasks.Enabled {
		orch, err = tasks.NewOrchestrator(cfg.Tasks, registry)
		if err != nil {
			return nil, fmt.Errorf("orchestrator: %w", err)
		}
		redisHealth = orch.HealthCheck()
		worker, err = tasks.NewWorker(cfg.Tasks, orch, registry, logger)
		if err != nil {
			return nil, fmt.Errorf("worker: %w", err)
		}
	}

	handler, err := httptransport.NewRouter(httptransport.RouterDeps{
		Config:       cfg,
		Logger:       logger,
		Metrics:      metrics,
		Registry:     registry,
		Orchestrator: orch,
		RedisHealth:  redisHealth,
	})
	if err != nil {
		return nil, fmt.Errorf("router: %w", err)
	}

	server, err := httptransport.NewServer(cfg.Server, handler, logger)
	if err != nil {
		return nil, fmt.Errorf("server: %w", err)
	}

	return &Application{
		cfg:           cfg,
		logger:        logger,
		server:        server,
		worker:        worker,
		orchestrator:  orch,
		traceShutdown: traceShutdown,
	}, nil
}

// buildRegistry iterates over the user-defined engines map and
// constructs each adapter from its compat_type. Adding a new
// compat_type is one switch arm here plus a new package under
// internal/engine/compat/.
func buildRegistry(cfg *config.Config) (engine.Registry, error) {
	entries := map[engine.Name]engine.RegistryEntry{}
	for name, ec := range cfg.Engines {
		if !ec.Enable {
			continue
		}
		client, err := httpclient.New(ec)
		if err != nil {
			return nil, fmt.Errorf("engine %s: httpclient: %w", name, err)
		}
		br := breaker.New(name, ec.Breaker)
		ad, err := newAdapterByCompat(engine.Name(name), ec, client, br)
		if err != nil {
			return nil, fmt.Errorf("engine %s: %w", name, err)
		}
		entries[engine.Name(name)] = engine.RegistryEntry{Engine: ad, MimeTypes: ec.MimeTypes}
	}
	if len(entries) == 0 {
		return nil, errors.New("no engines enabled")
	}
	return engine.NewRegistry(entries, engine.Name(cfg.Routing.DefaultEngine))
}

func newAdapterByCompat(name engine.Name, ec config.EngineConfig, client *httpclient.Client, br *breaker.Breaker) (engine.Engine, error) {
	switch ec.CompatType {
	case config.CompatDocling:
		return docling.New(name, ec, client, br)
	case config.CompatExternal:
		return external.New(name, ec, client, br)
	case config.CompatTika:
		return tika.New(name, ec, client, br)
	case config.CompatDoclingExternal:
		return doclingexternal.New(name, ec, client, br)
	default:
		return nil, fmt.Errorf("unknown compat_type %q", ec.CompatType)
	}
}

// Run starts the HTTP server and (optional) async worker. It blocks
// until ctx is cancelled or a fatal error occurs, then performs an
// ordered shutdown.
func (a *Application) Run(ctx context.Context) error {
	errCh := make(chan error, 2)
	go func() {
		if err := a.server.ListenAndServe(); err != nil && !errors.Is(err, errContextDone) {
			errCh <- fmt.Errorf("server: %w", err)
		}
	}()
	if a.worker != nil {
		go func() {
			if err := a.worker.Start(); err != nil {
				errCh <- fmt.Errorf("worker: %w", err)
			}
		}()
	}

	select {
	case <-ctx.Done():
		a.logger.Info().Msg("shutdown_signal_received")
	case err := <-errCh:
		a.logger.Error().Err(err).Msg("subsystem_failed")
	}

	return a.shutdown()
}

func (a *Application) shutdown() error {
	grace := a.server.GracePeriod()
	ctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()

	if a.worker != nil {
		a.worker.Shutdown()
	}
	if a.orchestrator != nil {
		_ = a.orchestrator.Close()
	}
	if a.traceShutdown != nil {
		_ = a.traceShutdown(ctx)
	}
	if err := a.server.Shutdown(ctx); err != nil {
		return fmt.Errorf("server shutdown: %w", err)
	}
	a.logger.Info().Dur("grace", grace).Msg("shutdown_complete")
	return nil
}

// errContextDone is a sentinel matching http.ErrServerClosed semantics.
var errContextDone = errors.New("context done")
