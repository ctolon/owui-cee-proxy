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
	// L4: passthrough mount failures bubble out as a NewRouter error
	// rather than panic. Captured in mountErr by the authenticated
	// subtree closure and returned below.
	var mountErr error

	// Global middleware (order matters).
	//
	// Layering rationale:
	//   - Recover first: a panic anywhere downstream — including in
	//     RequestID's UUID generator on a broken /dev/urandom — must
	//     not bring down the process. Recover is cheap so cost is
	//     negligible (M12).
	//   - RequestID next: every later middleware (logs, metrics,
	//     traces) wants a stable correlation ID.
	//   - BodyLimit before AccessLog/Metrics: oversize requests
	//     should fail fast and NOT pollute access logs / RED counters
	//     with traffic we never intended to serve (M10).
	r.Use(mw.Recover(d.Logger))
	r.Use(mw.RequestID())
	r.Use(mw.OTel(d.Config.Observability.Tracing.ServiceName))
	r.Use(mw.GlobalRateLimit(d.Config.RateLimitGlobal.RPS, d.Config.RateLimitGlobal.Burst))
	r.Use(mw.BodyLimit(d.Config.Server.MaxBodyBytes))
	r.Use(mw.AccessLog(d.Logger))
	r.Use(mw.Metrics(d.Metrics))

	// Operational endpoints (always unauthenticated for kubelet probes).
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
		// M1: every authenticated handler runs under a request-scoped
		// deadline. Streaming convert / passthrough handlers honor
		// r.Context() and abort cleanly on deadline expiry.
		r.Use(mw.Timeout(d.Config.Server.RequestTimeout))
		r.Use(mw.APIKey(
			d.Config.Security.ProxyAPIKeyHeader,
			secretsToStrings(d.Config.Security.ProxyAPIKeys),
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
		mountErr = mountPassthrough(r, d, engine.Docling, d.Config.Routing.Passthrough.DoclingPrefix, d.Config.Engines.Docling)
		if mountErr != nil {
			return
		}
		mountErr = mountPassthrough(r, d, engine.Tika, d.Config.Routing.Passthrough.TikaPrefix, d.Config.Engines.Tika)
		if mountErr != nil {
			return
		}
		mountErr = mountPassthrough(r, d, engine.Kreuzberg, d.Config.Routing.Passthrough.KreuzbergPrefix, d.Config.Engines.Kreuzberg)
	})

	if mountErr != nil {
		return nil, mountErr
	}
	return r, nil
}

func mountPassthrough(r chi.Router, d RouterDeps, name engine.Name, prefix string, ec config.EngineConfig) error {
	if !ec.Enable || prefix == "" {
		return nil
	}
	apiKeyHeader := apiKeyHeaderFor(name)
	rp, err := reverseproxy.New(ec.URL, prefix, apiKeyHeader, string(ec.APIKey), nil, d.Config.Security.TrustedProxies)
	if err != nil {
		return err
	}
	pt := &handlers.Passthrough{Engine: name, Proxy: rp, URL: ec.URL}
	// M3 defence-in-depth: wrap with RejectTraversal so a request whose
	// raw path contains "../" is 400'd before reaching the proxy.
	r.Handle(path.Clean(prefix)+"/*", reverseproxy.RejectTraversal(pt))
	return nil
}

// secretsToStrings widens a []config.Secret into a []string for callers
// that want the raw values (e.g. constant-time compares against an
// inbound header). Casts are explicit so it's obvious the redaction
// guarantee of Secret is intentionally bypassed at the call site.
func secretsToStrings(in []config.Secret) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = string(s)
	}
	return out
}

func apiKeyHeaderFor(name engine.Name) string {
	switch name {
	case engine.Kreuzberg:
		return "Authorization"
	default:
		return "X-Api-Key"
	}
}
