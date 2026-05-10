package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/ctolon/owui-cee-proxy/internal/engine"
	"github.com/ctolon/owui-cee-proxy/internal/version"
)

type Health struct {
	Registry engine.Registry
	Redis    func(context.Context) error // nil = no Redis configured
}

func (h *Health) Liveness(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"build":  version.Current(),
	})
}

func (h *Health) Readiness(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	results := map[string]string{}
	overall := http.StatusOK
	for _, name := range h.Registry.Names() {
		e, _ := h.Registry.Get(name)
		if err := e.Health(ctx); err != nil {
			results[string(name)] = "unhealthy: " + err.Error()
			overall = http.StatusServiceUnavailable
			continue
		}
		results[string(name)] = "ok"
	}
	if h.Redis != nil {
		if err := h.Redis(ctx); err != nil {
			results["redis"] = "unhealthy: " + err.Error()
			overall = http.StatusServiceUnavailable
		} else {
			results["redis"] = "ok"
		}
	}
	writeJSON(w, overall, map[string]any{
		"status":  statusFor(overall),
		"engines": results,
	})
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
