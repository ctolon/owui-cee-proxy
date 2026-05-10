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
	Config        *config.Config
	Logger        zerolog.Logger
	Metrics       *observability.Metrics
	Registry      engine.Registry
	Orchestrator  *tasks.Orchestrator
	RedisHealth   func(context.Context) error
}

func NewRouter(d RouterDeps) (http.Handler, error) {
	r := chi.NewRouter()

	// Global middleware (order matters).
	r.Use(mw.RequestID())
	r.Use(mw.Recover(d.Logger))
	r.Use(mw.OTel(d.Config.Observability.Tracing.ServiceName))
	r.Use(mw.GlobalRateLimit(d.Config.RateLimitGlobal.RPS, d.Config.RateLimitGlobal.Burst))
	r.Use(mw.AccessLog(d.Logger))
	r.Use(mw.Metrics(d.Metrics))
	r.Use(mw.BodyLimit(d.Config.Server.MaxBodyBytes))

	// Operational endpoints (always unauthenticated for kubelet probes).
	health := &handlers.Health{Registry: d.Registry, Redis: d.RedisHealth}
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
		r.Use(mw.APIKey(
			d.Config.Security.ProxyAPIKeyHeader,
			d.Config.Security.ProxyAPIKeys,
			d.Config.Security.RequireAPIKey,
		))

		// Docling-compatible facade.
		facade := strings.TrimRight(d.Config.Routing.FacadePathPrefix, "/")
		if facade == "" {
			facade = "/v1"
		}
		convert := &handlers.Convert{Registry: d.Registry}
		r.Post(facade+"/convert/file", convert.File)
		r.Post(facade+"/convert/source", convert.Source)

		async := &handlers.Async{Registry: d.Registry, Orchestrator: d.Orchestrator}
		r.Post(facade+"/convert/file/async", async.SubmitFile)
		r.Post(facade+"/convert/source/async", async.SubmitSource)
		r.Get(facade+"/status/poll/{id}", async.Poll)
		r.Get(facade+"/result/{id}", async.Result)

		// Native passthrough mounts.
		if err := mountPassthrough(r, d, engine.Docling, d.Config.Routing.Passthrough.DoclingPrefix, d.Config.Engines.Docling); err != nil {
			panic(err)
		}
		if err := mountPassthrough(r, d, engine.Tika, d.Config.Routing.Passthrough.TikaPrefix, d.Config.Engines.Tika); err != nil {
			panic(err)
		}
		if err := mountPassthrough(r, d, engine.Kreuzberg, d.Config.Routing.Passthrough.KreuzbergPrefix, d.Config.Engines.Kreuzberg); err != nil {
			panic(err)
		}
	})

	return r, nil
}

func mountPassthrough(r chi.Router, d RouterDeps, name engine.Name, prefix string, ec config.EngineConfig) error {
	if !ec.Enable || prefix == "" {
		return nil
	}
	apiKeyHeader := apiKeyHeaderFor(name)
	rp, err := reverseproxy.New(ec.URL, prefix, apiKeyHeader, ec.APIKey, nil)
	if err != nil {
		return err
	}
	pt := &handlers.Passthrough{Engine: name, Proxy: rp, URL: ec.URL}
	r.Handle(path.Clean(prefix)+"/*", pt)
	return nil
}

func apiKeyHeaderFor(name engine.Name) string {
	switch name {
	case engine.Kreuzberg:
		return "Authorization"
	default:
		return "X-Api-Key"
	}
}
