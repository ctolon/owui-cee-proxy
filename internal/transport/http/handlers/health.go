package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"golang.org/x/sync/errgroup"

	"github.com/ctolon/owui-cee-proxy/internal/engine"
	mw "github.com/ctolon/owui-cee-proxy/internal/transport/http/middleware"
	"github.com/ctolon/owui-cee-proxy/internal/version"
)

// Tunables exported for operator-facing configuration. Treat these
// as defaults; observability.health.* in YAML overrides them
// per-deployment.
const (
	// DefaultReadinessTimeout caps the overall readiness probe at the
	// kubelet's typical per-probe budget for an upload-heavy service.
	DefaultReadinessTimeout = 5 * time.Second
	// DefaultProbeParallelism bounds the engine Health() fan-out.
	// 16 covers realistic engine counts; bump via
	// observability.health.max_parallel for >16-engine deployments
	// where the previous hard-coded 8 would have caused kubelet
	// probe timeouts on serial fan-out.
	DefaultProbeParallelism = 16
)

// HealthMetrics is the observability seam for /readyz. Nil callers
// (tests) skip metric recording. Production wires this to the
// HealthProbeTotal counter + HealthProbeDuration histogram.
type HealthMetrics interface {
	RecordHealthProbe(engine, status string, duration time.Duration)
}

// Health serves /healthz (always 200) and /readyz (per-engine probes).
//
// M4: the readyz response body never includes upstream error strings —
// they can leak internal hostnames, backend URLs, or breaker state.
// The full error is logged via the configured Logger; the response
// body always reads the literal "unhealthy" so callers cannot fingerprint
// the failure mode. Logger defaults to zerolog.Nop() for callers that
// construct Health without observability wired up.
type Health struct {
	Registry engine.Registry
	Redis    func(context.Context) error // nil = no Redis configured
	Logger   zerolog.Logger
	// SilenceEngineHealth, when true, marks every engine probe ctx
	// as log-silent so the instrumented HTTP transport drops its
	// outbound_engine_request debug line. Status counters still fire.
	SilenceEngineHealth bool
	// Timeout caps the readiness probe. 0 → DefaultReadinessTimeout.
	Timeout time.Duration
	// MaxParallel bounds the fan-out goroutine count.
	// 0 → DefaultProbeParallelism.
	MaxParallel int
	// Metrics surfaces per-engine probe outcomes + durations.
	Metrics HealthMetrics
}

func (h *Health) Liveness(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"build":  version.Current(),
	})
}

// Readiness probes every enabled engine and (optionally) Redis in
// parallel, capped at h.MaxParallel goroutines. Errors from one
// engine never block another.
func (h *Health) Readiness(w http.ResponseWriter, r *http.Request) {
	timeout := h.Timeout
	if timeout <= 0 {
		timeout = DefaultReadinessTimeout
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	if h.SilenceEngineHealth {
		ctx = mw.WithSilenceLog(ctx)
	}

	results := map[string]string{}
	var mu sync.Mutex

	g, gctx := errgroup.WithContext(ctx)
	limit := h.MaxParallel
	if limit <= 0 {
		limit = DefaultProbeParallelism
	}
	g.SetLimit(limit)

	for _, name := range h.Registry.Names() {
		name := name
		g.Go(func() error {
			start := time.Now()
			e, err := h.Registry.Get(name)
			if err != nil {
				h.logProbeError(string(name), err)
				h.recordProbe(string(name), "unhealthy", time.Since(start))
				mu.Lock()
				results[string(name)] = "unhealthy"
				mu.Unlock()
				return nil
			}
			if err := e.Health(gctx); err != nil {
				h.logProbeError(string(name), err)
				h.recordProbe(string(name), "unhealthy", time.Since(start))
				mu.Lock()
				results[string(name)] = "unhealthy"
				mu.Unlock()
				return nil
			}
			h.recordProbe(string(name), "ok", time.Since(start))
			mu.Lock()
			results[string(name)] = "ok"
			mu.Unlock()
			return nil
		})
	}

	if h.Redis != nil {
		g.Go(func() error {
			start := time.Now()
			if err := h.Redis(gctx); err != nil {
				h.logProbeError("redis", err)
				h.recordProbe("redis", "unhealthy", time.Since(start))
				mu.Lock()
				results["redis"] = "unhealthy"
				mu.Unlock()
				return nil
			}
			h.recordProbe("redis", "ok", time.Since(start))
			mu.Lock()
			results["redis"] = "ok"
			mu.Unlock()
			return nil
		})
	}

	// errgroup never returns a non-nil error here (we always return
	// nil from goroutines), but call Wait for the synchronization.
	_ = g.Wait()

	overall := http.StatusOK
	for _, v := range results {
		if v != "ok" {
			overall = http.StatusServiceUnavailable
			break
		}
	}

	writeJSON(w, overall, map[string]any{
		"status":  statusFor(overall),
		"engines": results,
	})
}

// logProbeError records the full upstream error for operator
// debugging without leaking it into the response body. M4.
func (h *Health) logProbeError(probe string, err error) {
	h.Logger.Warn().
		Err(err).
		Str("event", "readyz_probe_failed").
		Str("probe", probe).
		Msg("readiness probe failed")
}

// recordProbe fires the Prometheus seam if Metrics is wired. Tests
// without metrics get a free pass.
func (h *Health) recordProbe(engineName, status string, dur time.Duration) {
	if h.Metrics == nil {
		return
	}
	h.Metrics.RecordHealthProbe(engineName, status, dur)
}

func statusFor(s int) string {
	if s == http.StatusOK {
		return "ready"
	}
	return "not-ready"
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
