// Package app is the composition root. It wires the configuration into
// concrete adapter implementations and returns a runnable Application.
// The HTTP transport, async worker, and observability subsystems all
// receive their dependencies through this single seam.
package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rs/zerolog"

	"github.com/ctolon/owui-cee-proxy/internal/breaker"
	"github.com/ctolon/owui-cee-proxy/internal/config"
	"github.com/ctolon/owui-cee-proxy/internal/engine"
	"github.com/ctolon/owui-cee-proxy/internal/engine/authutil"
	"github.com/ctolon/owui-cee-proxy/internal/engine/compat/docling"
	"github.com/ctolon/owui-cee-proxy/internal/engine/compat/doclingexternal"
	"github.com/ctolon/owui-cee-proxy/internal/engine/compat/external"
	"github.com/ctolon/owui-cee-proxy/internal/engine/compat/tika"
	"github.com/ctolon/owui-cee-proxy/internal/engine/mimedetect"
	"github.com/ctolon/owui-cee-proxy/internal/httpclient"
	"github.com/ctolon/owui-cee-proxy/internal/observability"
	"github.com/ctolon/owui-cee-proxy/internal/tasks"
	httptransport "github.com/ctolon/owui-cee-proxy/internal/transport/http"
	"github.com/ctolon/owui-cee-proxy/internal/transport/http/handlers"
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

	metrics := observability.NewMetrics()

	registry, err := buildRegistry(cfg, logger, metrics)
	if err != nil {
		return nil, err
	}

	// Surface auth-resolution outcomes at bootstrap so operators don't
	// debug silent 401s in production. Each engine gets one line; any
	// asymmetric config (auth_header set + api_key_env missing, etc.)
	// emerges here.
	logEngineBootstrap(logger, cfg)

	var orch *tasks.Orchestrator
	var worker *tasks.Worker
	var redisHealth func(context.Context) error
	if cfg.Tasks.Enabled {
		orch, err = tasks.NewOrchestrator(cfg.Tasks, registry)
		if err != nil {
			return nil, fmt.Errorf("orchestrator: %w", err)
		}
		orch.WithObservability(
			logger,
			&taskLifecycleAdapter{m: metrics},
			&queueDepthAdapter{m: metrics},
		)
		redisHealth = orch.HealthCheck()
		worker, err = tasks.NewWorker(cfg.Tasks, orch, registry, logger)
		if err != nil {
			return nil, fmt.Errorf("worker: %w", err)
		}
	}

	resolver := handlers.NewResolver(
		cfg.Security.SSRF.DNSCacheTTL,
		cfg.Security.SSRF.DNSCacheMax,
		&ssrfMetricsAdapter{m: metrics},
		&ssrfLoggerAdapter{logger: logger},
	)

	// Single process-wide MIME resolver. The map merge happens here,
	// once; handlers + tasks share the same instance for free.
	mimeResolver := mimedetect.NewResolver(cfg.Mimedetect.ExtensionOverrides)
	logRoutingBootstrap(logger, cfg, registry, len(cfg.Mimedetect.ExtensionOverrides))

	handler, err := httptransport.NewRouter(httptransport.RouterDeps{
		Config:        cfg,
		Logger:        logger,
		Metrics:       metrics,
		Registry:      registry,
		Orchestrator:  orch,
		RedisHealth:   redisHealth,
		SSRFResolver:  resolver,
		HealthMetrics: &healthMetricsAdapter{m: metrics},
		MimeResolver:  mimeResolver,
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
//
// Each per-engine httpclient.Client is wired with:
//   - AuthMetrics  → authutil.Apply records auth-stamping outcomes
//   - Instrument() → outbound RoundTripper logs every request at debug
//     and records EngineUpstreamStatus on the response
//
// Tests bypass app.go and construct adapters directly, so the
// metric/log seams degrade to Nop when not wired (see httpclient.Client.Auth).
func buildRegistry(cfg *config.Config, logger zerolog.Logger, metrics *observability.Metrics) (engine.Registry, error) {
	authM := &authMetricsAdapter{m: metrics}
	upstreamM := &upstreamStatusAdapter{m: metrics}
	entries := map[engine.Name]engine.RegistryEntry{}
	for name, ec := range cfg.Engines {
		if !ec.Enable {
			continue
		}
		client, err := httpclient.New(ec)
		if err != nil {
			return nil, fmt.Errorf("engine %s: httpclient: %w", name, err)
		}
		client.
			WithAuthMetrics(authM).
			Instrument(httpclient.InstrumentOptions{
				Logger:       logger,
				Recorder:     upstreamM,
				EngineName:   name,
				AuthHeader:   ec.AuthHeader,
				AuthScheme:   string(ec.AuthScheme),
				BodyLogBytes: ec.ResolveUpstream(cfg.Observability.Upstream).BodyLogBytes,
			})
		br := breaker.New(name, ec.Breaker, breakerStateHook(logger, metrics))
		ad, err := newAdapterByCompat(engine.Name(name), ec, client, br)
		if err != nil {
			return nil, fmt.Errorf("engine %s: %w", name, err)
		}
		entries[engine.Name(name)] = engine.RegistryEntry{
			Engine:     ad,
			MimeTypes:  ec.MimeTypes,
			Extensions: ec.Extensions,
		}
	}
	if len(entries) == 0 {
		return nil, errors.New("no engines enabled")
	}
	return engine.NewRegistry(entries, engine.Name(cfg.Routing.DefaultEngine), cfg.Routing.Strategy)
}

// logEngineBootstrap emits one info line per enabled engine + one warn
// per non-fatal misconfiguration discovered at load time. Operators see
// these in `kubectl logs` immediately after a rollout; together they
// answer "did my Secret reach the Pod?" without per-request log
// archaeology.
func logEngineBootstrap(logger zerolog.Logger, cfg *config.Config) {
	for name, ec := range cfg.Engines {
		if !ec.Enable {
			continue
		}
		facades := config.AcceptedFacades(ec.CompatType)
		logger.Info().
			Str("event", "engine_initialised").
			Str("engine", name).
			Str("compat_type", string(ec.CompatType)).
			Str("url", ec.URL).
			Str("api_key_env", ec.APIKeyEnv).
			Bool("api_key_resolved", ec.APIKey != "").
			Int("api_key_len", len(string(ec.APIKey))).
			Str("auth_header", ec.AuthHeader).
			Str("auth_scheme", string(ec.AuthScheme)).
			Strs("facades", facades).
			Int("mime_types_count", len(ec.MimeTypes)).
			Int("extensions_count", len(ec.Extensions)).
			Msg("engine initialised")
	}
	for _, w := range cfg.AuthWarnings() {
		logger.Warn().
			Str("event", "engine_auth_warning").
			Msg(w)
	}
}

// logRoutingBootstrap emits a single record describing the effective
// routing strategy + the number of operator-supplied MIME extension
// overrides. Surface so operators can verify the YAML knobs landed
// without diffing a logback config.
func logRoutingBootstrap(logger zerolog.Logger, cfg *config.Config, reg engine.Registry, overrideCount int) {
	logger.Info().
		Str("event", "routing_strategy_initialised").
		Str("configured_strategy", string(cfg.Routing.Strategy)).
		Str("effective_strategy", string(reg.Strategy())).
		Str("default_engine", cfg.Routing.DefaultEngine).
		Int("mime_extension_overrides_count", overrideCount).
		Msg("routing strategy initialised")
}

// breakerStateHook returns the OnStateChange callback bound to the
// project's Prometheus gauge + structured logger. Lives here at the
// composition root so internal/breaker stays observability-agnostic.
//
// The hook is purely additive: it does NOT influence the breaker's
// state machine, only surfaces transitions to operators. Logged at
// info level — half-open → open transitions warrant an alert
// dashboard, not a paging line.
func breakerStateHook(logger zerolog.Logger, metrics *observability.Metrics) breaker.StateChangeFunc {
	return func(name, from, to string) {
		if metrics != nil && metrics.BreakerStateGauge != nil {
			metrics.BreakerStateGauge.WithLabelValues(name).Set(breaker.StateValue(to))
		}
		logger.Info().
			Str("event", "breaker_state_changed").
			Str("engine", name).
			Str("from", from).
			Str("to", to).
			Msg("circuit breaker state changed")
	}
}

// authMetricsAdapter bridges the authutil.AuthMetrics interface to
// the Prometheus counter. Lives here (composition root) so the
// engine/authutil package stays Prometheus-agnostic.
type authMetricsAdapter struct{ m *observability.Metrics }

func (a *authMetricsAdapter) RecordAuthAttempt(engineName, scheme string, outcome authutil.Outcome) {
	if a.m == nil || a.m.EngineAuthAttached == nil {
		return
	}
	a.m.EngineAuthAttached.WithLabelValues(engineName, scheme, string(outcome)).Inc()
}

// ssrfMetricsAdapter bridges handlers.SSRFMetrics to the SSRFRejected
// + SSRFDNSCache Prometheus counters. The handlers package stays
// metric-agnostic; metric label values come from the typed
// SSRFRejectReason taxonomy.
type ssrfMetricsAdapter struct{ m *observability.Metrics }

func (s *ssrfMetricsAdapter) RecordRejection(reason handlers.SSRFRejectReason) {
	if s.m == nil || s.m.SSRFRejected == nil {
		return
	}
	s.m.SSRFRejected.WithLabelValues(string(reason)).Inc()
}

func (s *ssrfMetricsAdapter) RecordDNSCacheHit() {
	if s.m == nil || s.m.SSRFDNSCache == nil {
		return
	}
	s.m.SSRFDNSCache.WithLabelValues("hit").Inc()
}

func (s *ssrfMetricsAdapter) RecordDNSCacheMiss() {
	if s.m == nil || s.m.SSRFDNSCache == nil {
		return
	}
	s.m.SSRFDNSCache.WithLabelValues("miss").Inc()
}

func (s *ssrfMetricsAdapter) RecordDNSCacheBypass() {
	if s.m == nil || s.m.SSRFDNSCache == nil {
		return
	}
	s.m.SSRFDNSCache.WithLabelValues("bypass").Inc()
}

// ssrfLoggerAdapter writes a warn-level log line for every SSRF
// rejection. The raw URL is logged as-is (not redacted) because it
// is operator-facing audit data; the reason is what dashboards
// alert on, not the URL.
type ssrfLoggerAdapter struct{ logger zerolog.Logger }

func (l *ssrfLoggerAdapter) LogRejection(host, raw string, reason handlers.SSRFRejectReason, err error) {
	ev := l.logger.Warn().
		Str("event", "ssrf_rejected").
		Str("reason", string(reason)).
		Str("host", host).
		Str("url", raw)
	if err != nil {
		ev = ev.Err(err)
	}
	ev.Msg("SSRF policy rejected URL")
}

// taskLifecycleAdapter bridges tasks.LifecycleRecorder to the
// TaskLifecycle Prometheus counter.
type taskLifecycleAdapter struct{ m *observability.Metrics }

func (a *taskLifecycleAdapter) RecordTaskLifecycle(stage, outcome string) {
	if a.m == nil || a.m.TaskLifecycle == nil {
		return
	}
	a.m.TaskLifecycle.WithLabelValues(stage, outcome).Inc()
}

// queueDepthAdapter bridges tasks.QueueDepthRecorder to the
// WorkerQueueDepth gauge.
type queueDepthAdapter struct{ m *observability.Metrics }

func (a *queueDepthAdapter) SetQueueDepth(queue string, depth int) {
	if a.m == nil || a.m.WorkerQueueDepth == nil {
		return
	}
	a.m.WorkerQueueDepth.WithLabelValues(queue).Set(float64(depth))
}

// healthMetricsAdapter bridges handlers.HealthMetrics to the
// HealthProbeTotal + HealthProbeDuration collectors.
type healthMetricsAdapter struct{ m *observability.Metrics }

func (h *healthMetricsAdapter) RecordHealthProbe(engineName, status string, dur time.Duration) {
	if h.m == nil {
		return
	}
	if h.m.HealthProbeTotal != nil {
		h.m.HealthProbeTotal.WithLabelValues(engineName, status).Inc()
	}
	if h.m.HealthProbeDuration != nil {
		h.m.HealthProbeDuration.WithLabelValues(engineName).Observe(dur.Seconds())
	}
}

// upstreamStatusAdapter bridges httpclient.UpstreamStatusRecorder to
// the EngineUpstreamStatus counter. Same rationale as above.
type upstreamStatusAdapter struct{ m *observability.Metrics }

func (u *upstreamStatusAdapter) RecordUpstreamStatus(engineName string, status int) {
	if u.m == nil || u.m.EngineUpstreamStatus == nil {
		return
	}
	u.m.EngineUpstreamStatus.WithLabelValues(engineName, observability.StatusClass(status)).Inc()
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
	// Queue-depth poller: only runs when tasks are enabled. The
	// 10s cadence is a balance between dashboard freshness and
	// Redis scrape cost; bumping much faster than that adds load
	// without operator benefit.
	if a.orchestrator != nil {
		go a.runQueueDepthPoller(ctx)
	}

	select {
	case <-ctx.Done():
		a.logger.Info().Msg("shutdown_signal_received")
	case err := <-errCh:
		a.logger.Error().Err(err).Msg("subsystem_failed")
	}

	return a.shutdown()
}

// runQueueDepthPoller emits the WorkerQueueDepth gauge every 10s. The
// poll is best-effort: a Redis hiccup logs at debug, doesn't fail the
// proxy. Stops cleanly on ctx cancellation as part of normal shutdown.
func (a *Application) runQueueDepthPoller(ctx context.Context) {
	const period = 10 * time.Second
	ticker := time.NewTicker(period)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.orchestrator.PollQueueDepth(ctx)
		}
	}
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
