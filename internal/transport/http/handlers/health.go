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
	"github.com/ctolon/owui-cee-proxy/internal/version"
)

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
}

func (h *Health) Liveness(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"build":  version.Current(),
	})
}

// Readiness probes every enabled engine and (optionally) Redis in
// parallel. The 5-second context deadline bounds the worst case so a
// hung backend cannot delay the kubelet probe past the configured
// timeout window. Errors from one engine never block another.
func (h *Health) Readiness(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	results := map[string]string{}
	var mu sync.Mutex

	g, gctx := errgroup.WithContext(ctx)
	// Cap the goroutine count to a sensible default. With at most a
	// handful of engines + Redis this is essentially a no-op, but it
	// guards against future growth of the registry.
	g.SetLimit(8)

	for _, name := range h.Registry.Names() {
		name := name
		g.Go(func() error {
			e, err := h.Registry.Get(name)
			if err != nil {
				h.logProbeError(string(name), err)
				mu.Lock()
				results[string(name)] = "unhealthy"
				mu.Unlock()
				// Do NOT return err: we want every probe to run to
				// completion regardless of any single failure.
				return nil
			}
			if err := e.Health(gctx); err != nil {
				h.logProbeError(string(name), err)
				mu.Lock()
				results[string(name)] = "unhealthy"
				mu.Unlock()
				return nil
			}
			mu.Lock()
			results[string(name)] = "ok"
			mu.Unlock()
			return nil
		})
	}

	if h.Redis != nil {
		g.Go(func() error {
			if err := h.Redis(gctx); err != nil {
				h.logProbeError("redis", err)
				mu.Lock()
				results["redis"] = "unhealthy"
				mu.Unlock()
				return nil
			}
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
