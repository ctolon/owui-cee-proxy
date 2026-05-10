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
}

func NewRouter(d RouterDeps) (http.Handler, error) {
	r := chi.NewRouter()
	var mountErr error

	r.Use(mw.Recover(d.Logger))
	r.Use(mw.RequestID())
	r.Use(mw.OTel(d.Config.Observability.Tracing.ServiceName))
	r.Use(mw.GlobalRateLimit(d.Config.RateLimitGlobal.RPS, d.Config.RateLimitGlobal.Burst))
	r.Use(mw.BodyLimit(d.Config.Server.MaxBodyBytes))
	r.Use(mw.AccessLog(d.Logger))
	r.Use(mw.Metrics(d.Metrics))

	health := &handlers.Health{Registry: d.Registry, Redis: d.RedisHealth, Logger: d.Logger}
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

		// Docling facade — POST /v1/convert/{file,source} plus async siblings.
		if d.Config.Routing.Facade.Docling.Enabled {
			facade := strings.TrimRight(d.Config.Routing.Facade.Docling.Prefix, "/")
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
		}

		// External facade — PUT /process raw + page_content shape.
		if d.Config.Routing.Facade.External.Enabled {
			processPath := d.Config.Routing.Facade.External.Path
			if processPath == "" {
				processPath = "/process"
			}
			ext := &handlers.External{Registry: d.Registry, MaxBytes: d.Config.Server.MaxBodyBytes}
			r.Put(processPath, ext.Process)
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

func mountEnginePassthrough(r chi.Router, d RouterDeps, name string, ec config.EngineConfig) error {
	prefix := "/" + name
	rp, err := reverseproxy.New(ec.URL, prefix, ec.AuthHeader, string(ec.APIKey), nil, d.Config.Security.TrustedProxies)
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
