package http

import (
	"context"
	"net/http"
	"path"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog"

	"github.com/ctolon/owui-cee-proxy/internal/config"
	"github.com/ctolon/owui-cee-proxy/internal/engine"
	"github.com/ctolon/owui-cee-proxy/internal/observability"
	"github.com/ctolon/owui-cee-proxy/internal/tasks"
	"github.com/ctolon/owui-cee-proxy/internal/transport/http/handlers"
	mw "github.com/ctolon/owui-cee-proxy/internal/transport/http/middleware"
	"github.com/ctolon/owui-cee-proxy/internal/transport/http/reverseproxy"
)

type RouterDeps struct {
	Config       *config.Config
	Logger       zerolog.Logger
	Metrics      *observability.Metrics
	Registry     engine.Registry
	Orchestrator *tasks.Orchestrator
	RedisHealth  func(context.Context) error
	// SSRFResolver applies the /v1/convert/source SSRF policy. The
	// composition root wires this with cache + metrics + logger; if
	// left nil, mountFacadeDocling falls back to the package's
	// zero-config Resolver (no cache, no observability).
	SSRFResolver *handlers.Resolver
	// HealthMetrics records per-engine readiness probe outcomes +
	// durations. Nil disables metric recording (tests).
	HealthMetrics handlers.HealthMetrics
}

func NewRouter(d RouterDeps) (http.Handler, error) {
	r := chi.NewRouter()
	var mountErr error

	r.Use(mw.Recover(d.Logger))
	r.Use(mw.RequestID())
	r.Use(mw.OTel(d.Config.Observability.Tracing.ServiceName))
	r.Use(mw.GlobalRateLimit(d.Config.RateLimitGlobal.RPS, d.Config.RateLimitGlobal.Burst))
	r.Use(mw.BodyLimit(d.Config.Server.MaxBodyBytes))
	r.Use(mw.AccessLog(d.Logger, d.Config.Observability.Log.SilencePaths...))
	r.Use(mw.Metrics(d.Metrics))

	health := &handlers.Health{
		Registry:            d.Registry,
		Redis:               d.RedisHealth,
		Logger:              d.Logger,
		SilenceEngineHealth: d.Config.Observability.Log.SilenceEngineHealth,
		Timeout:             d.Config.Observability.Health.Timeout,
		MaxParallel:         d.Config.Observability.Health.MaxParallel,
		Metrics:             d.HealthMetrics,
	}
	r.Get("/healthz", health.Liveness)
	r.Get("/readyz", health.Readiness)

	if d.Config.Observability.Metrics.Enabled {
		metricsPath := d.Config.Observability.Metrics.Path
		if metricsPath == "" {
			metricsPath = "/metrics"
		}
		r.Handle(metricsPath, promhttp.HandlerFor(d.Metrics.Registry, promhttp.HandlerOpts{}))
	}

	// Authenticated subtree (facade + passthrough).
	r.Group(func(r chi.Router) {
		r.Use(mw.Timeout(d.Config.Server.RequestTimeout))
		r.Use(mw.APIKey(
			d.Config.Security.ProxyAPIKeyHeader,
			secretsToStrings(d.Config.Security.ProxyAPIKeys),
			d.Config.Security.RequireAPIKey,
		))

		if d.Config.Routing.Facade.Docling.Enabled {
			mountFacadeDocling(r, d)
		}
		if d.Config.Routing.Facade.External.Enabled {
			mountFacadeExternal(r, d)
		}

		// Native passthrough mounts: each enabled engine is reachable at
		// /<engine-name>/* using the engine's compat-driven auth header.
		if d.Config.Routing.Passthrough.Enabled {
			for name, ec := range d.Config.Engines {
				if !ec.Enable {
					continue
				}
				if err := mountEnginePassthrough(r, d, name, ec); err != nil {
					mountErr = err
					return
				}
			}
		}
	})

	if mountErr != nil {
		return nil, mountErr
	}
	return r, nil
}

// mountFacadeDocling wires the POST /v1/convert/{file,source} sync
// handlers plus their async siblings. Split out of NewRouter to keep
// the composition root readable.
func mountFacadeDocling(r chi.Router, d RouterDeps) {
	facade := strings.TrimRight(d.Config.Routing.Facade.Docling.Prefix, "/")
	if facade == "" {
		facade = "/v1"
	}
	convert := &handlers.Convert{
		Registry:    d.Registry,
		Logger:      d.Logger,
		UpstreamLog: resolveUpstreamLog(d.Config),
	}
	if d.SSRFResolver != nil {
		convert.ResolveURL = d.SSRFResolver.Resolve
	}
	r.Post(facade+"/convert/file", convert.File)
	r.Post(facade+"/convert/source", convert.Source)

	async := &handlers.Async{Registry: d.Registry, Orchestrator: d.Orchestrator}
	r.Post(facade+"/convert/file/async", async.SubmitFile)
	r.Post(facade+"/convert/source/async", async.SubmitSource)
	r.Get(facade+"/status/poll/{id}", async.Poll)
	r.Get(facade+"/result/{id}", async.Result)
}

// mountFacadeExternal wires the PUT /process external-loader path.
func mountFacadeExternal(r chi.Router, d RouterDeps) {
	processPath := d.Config.Routing.Facade.External.Path
	if processPath == "" {
		processPath = "/process"
	}
	ext := &handlers.External{
		Registry:    d.Registry,
		Logger:      d.Logger,
		MaxBytes:    d.Config.Server.MaxBodyBytes,
		UpstreamLog: resolveUpstreamLog(d.Config),
	}
	r.Put(processPath, ext.Process)
}

func mountEnginePassthrough(r chi.Router, d RouterDeps, name string, ec config.EngineConfig) error {
	prefix := "/" + name
	rp, err := reverseproxy.New(ec.URL, prefix, ec.AuthHeader, string(ec.APIKey), string(ec.AuthScheme), nil, d.Config.Security.TrustedProxies)
	if err != nil {
		return err
	}
	pt := &handlers.Passthrough{Engine: engine.Name(name), Proxy: rp, URL: ec.URL}
	r.Handle(path.Clean(prefix)+"/*", reverseproxy.RejectTraversal(pt))
	return nil
}

// secretsToStrings widens a []config.Secret into a []string for callers
// that want the raw values (constant-time compares against headers).
func secretsToStrings(in []config.Secret) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = string(s)
	}
	return out
}

// resolveUpstreamLog returns a closure that, given an engine name,
// produces the effective UpstreamLogConfig (global default merged
// with the engine's per-entry override). The handlers cache the
// closure once; per-request lookups are cheap (map read).
func resolveUpstreamLog(cfg *config.Config) func(engineName string) config.UpstreamLogConfig {
	global := cfg.Observability.Upstream
	return func(engineName string) config.UpstreamLogConfig {
		ec, ok := cfg.Engines[engineName]
		if !ok {
			return global
		}
		return ec.ResolveUpstream(global)
	}
}
