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
	"github.com/ctolon/owui-cee-proxy/internal/engine/authutil"
	"github.com/ctolon/owui-cee-proxy/internal/engine/mimedetect"
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
	// MimeResolver carries the merged built-in + operator-supplied
	// extension→MIME map; Convert, External, and Async share the
	// same instance so MIME resolution is identical across facades.
	MimeResolver *mimedetect.Resolver
	// EngineTransports is the per-engine raw *http.Transport map
	// keyed on engine name (the YAML map key). The passthrough mount
	// reaches into this map to reuse the engine's tuned connection
	// pool / TLS config; falling back to nil → http.DefaultTransport
	// would share the pool across every engine, violating the
	// per-engine isolation invariant (C-25 from REVIEW-FAANG.md).
	EngineTransports map[string]*http.Transport
	// PanicRecorder is the observability seam the Recover middleware
	// hits when it catches a panic. Composition root wires the
	// Prometheus counter (`panics_total{path}`); nil falls back to
	// the no-op recorder so tests that build a router without
	// metrics still work. (C-39 from REVIEW-FAANG.md.)
	PanicRecorder mw.PanicRecorder
}

func NewRouter(d RouterDeps) (http.Handler, error) {
	r := chi.NewRouter()
	var mountErr error

	r.Use(mw.Recover(d.Logger, d.PanicRecorder))
	r.Use(mw.RequestID())
	r.Use(mw.OTel(d.Config.Observability.Tracing.ServiceName))
	r.Use(mw.GlobalRateLimit(d.Config.RateLimitGlobal.RPS, d.Config.RateLimitGlobal.Burst))
	r.Use(mw.BodyLimit(d.Config.Server.MaxBodyBytes))
	r.Use(mw.AccessLog(d.Logger, d.Config.Observability.Log.SilencePaths...))
	r.Use(mw.Metrics(d.Metrics))
	// Optional sensitive audit-capture; default OFF. When enabled,
	// emits one `request_full_capture` debug record per request with
	// headers (redacted) + body prefix (base64). See
	// FullCaptureConfig for the redaction allowlist.
	r.Use(mw.FullCapture(d.Logger, mw.FullCaptureConfig{
		Enabled:            d.Config.Observability.Upstream.FullCapture,
		BodyBytes:          d.Config.Observability.Upstream.FullCaptureBodyBytes,
		RedactExtraHeaders: buildRedactHeaderList(d.Config),
	}))

	health := &handlers.Health{
		Registry:            d.Registry,
		Redis:               d.RedisHealth,
		Logger:              d.Logger,
		SilenceEngineHealth: d.Config.Observability.Log.SilenceEngineHealth,
		Timeout:             d.Config.Observability.Health.Timeout,
		MaxParallel:         d.Config.Observability.Health.MaxParallel,
		Metrics:             d.HealthMetrics,
		// Tiered readiness: default engine + Redis (when tasks are
		// enabled) are load-bearing. Non-default engines surface
		// as `degraded` in the body without flipping the pod 503.
		// (C-22 from REVIEW-FAANG.md.)
		Required: requiredReadinessNames(d.Config),
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
		Resolver:    d.MimeResolver,
		Fallback:    d.Config.Routing.Fallback.Enabled,
	}
	if d.SSRFResolver != nil {
		convert.ResolveURL = d.SSRFResolver.Resolve
	}
	r.Post(facade+"/convert/file", convert.File)
	r.Post(facade+"/convert/source", convert.Source)

	async := &handlers.Async{
		Registry:     d.Registry,
		Orchestrator: d.Orchestrator,
		Resolver:     d.MimeResolver,
		// APIKeyHeader is load-bearing: the orchestrator binds each
		// task token to the caller's API-key fingerprint and refuses
		// poll/result for any other caller. Without it set, the
		// fingerprint is empty for every request, so the binding
		// short-circuits and any holder of a task token can fetch any
		// task's result. Set it at wire time from the same knob that
		// drives the inbound APIKey middleware.
		APIKeyHeader: d.Config.Security.ProxyAPIKeyHeader,
	}
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
		Resolver:    d.MimeResolver,
	}
	r.Put(processPath, ext.Process)
}

// requiredReadinessNames returns the set of probe names whose health
// is load-bearing for the pod's readiness — the default engine plus
// "redis" when tasks are enabled. Non-required probes degrade the
// surface area without flipping the pod 503 (C-22).
func requiredReadinessNames(cfg *config.Config) map[string]struct{} {
	out := map[string]struct{}{}
	if cfg.Routing.DefaultEngine != "" {
		out[cfg.Routing.DefaultEngine] = struct{}{}
	}
	if cfg.Tasks.Enabled {
		out["redis"] = struct{}{}
	}
	return out
}

// buildRedactHeaderList collects every header name that should be
// redacted in the full-capture log: the global proxy API-key header
// plus every enabled engine's singular AuthHeader plus every
// AuthHeaders entry. Operators add custom credential header names
// here when they introduce a new compat_type that uses a non-default
// stamping convention.
func buildRedactHeaderList(cfg *config.Config) []string {
	out := make([]string, 0, 1+len(cfg.Engines)*2)
	if cfg.Security.ProxyAPIKeyHeader != "" {
		out = append(out, cfg.Security.ProxyAPIKeyHeader)
	}
	for _, ec := range cfg.Engines {
		if !ec.Enable {
			continue
		}
		if ec.AuthHeader != "" {
			out = append(out, ec.AuthHeader)
		}
		for _, ah := range ec.AuthHeaders {
			if ah.Header != "" {
				out = append(out, ah.Header)
			}
		}
	}
	return out
}

func mountEnginePassthrough(r chi.Router, d RouterDeps, name string, ec config.EngineConfig) error {
	prefix := "/" + name
	// Reuse the engine's per-instance *http.Transport (built by
	// httpclient.New from ec.HTTP — connection pool, TLS, dial
	// timeouts). Without this, reverseproxy.New(... nil ...) falls
	// back to http.DefaultTransport which shares its connection pool
	// across every engine, defeating the per-engine isolation
	// invariant. (C-25 from REVIEW-FAANG.md.)
	var tr http.RoundTripper
	if d.EngineTransports != nil {
		if t := d.EngineTransports[name]; t != nil {
			tr = t
		}
	}
	rp, err := reverseproxy.New(ec.URL, prefix, authutil.FormatHeaderValuesMulti(ec), tr, d.Config.Security.TrustedProxies)
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
