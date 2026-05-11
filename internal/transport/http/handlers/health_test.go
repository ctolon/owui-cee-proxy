package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ctolon/owui-cee-proxy/internal/engine"
)

// healthStubEngine implements engine.Engine with a configurable Health
// delay. Convert is unused by the readiness handler.
type healthStubEngine struct {
	name  engine.Name
	delay time.Duration
	err   error
}

func (s *healthStubEngine) Name() engine.Name { return s.name }
func (s *healthStubEngine) URL() string       { return "http://" + string(s.name) + ".test" }
func (s *healthStubEngine) Capabilities() engine.EngineCapabilities {
	return engine.EngineCapabilities{Facades: []engine.Facade{engine.FacadeDocling}}
}

func (s *healthStubEngine) Convert(_ context.Context, _ *engine.ConvertRequest) (*engine.ConvertResponse, error) {
	return nil, nil
}

func (s *healthStubEngine) Health(ctx context.Context) error {
	select {
	case <-time.After(s.delay):
		return s.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// healthStubRegistry implements engine.Registry with a fixed set of stubs.
type healthStubRegistry struct {
	engines map[engine.Name]engine.Engine
	order   []engine.Name
}

func (r *healthStubRegistry) Get(name engine.Name) (engine.Engine, error) {
	e, ok := r.engines[name]
	if !ok {
		return nil, engine.ErrUnknownEngine
	}
	return e, nil
}
func (r *healthStubRegistry) Default() engine.Engine { return r.engines[r.order[0]] }

func (r *healthStubRegistry) Pick(_ engine.Facade, _ string) engine.Engine {
	return r.engines[r.order[0]]
}

func (r *healthStubRegistry) PickWithError(_ engine.Facade, _ string) (engine.Engine, error) {
	return r.engines[r.order[0]], nil
}

func (r *healthStubRegistry) Names() []engine.Name { return r.order }

// PickRoute/PickRouteVerbose/Strategy satisfy the modern Registry
// interface for the readiness suite; tests don't exercise dispatch.
func (r *healthStubRegistry) PickRoute(_ engine.Facade, _ engine.PickHint) (engine.Engine, error) {
	return r.engines[r.order[0]], nil
}

func (r *healthStubRegistry) PickRouteVerbose(_ engine.Facade, _ engine.PickHint) (engine.Engine, engine.PickSource, error) {
	return r.engines[r.order[0]], engine.PickSourceDefault, nil
}

func (r *healthStubRegistry) Strategy() engine.RoutingStrategy {
	return engine.StrategyMimeThenExt
}

func TestReadiness_RunsHealthChecksConcurrently(t *testing.T) {
	t.Parallel()
	delay := 400 * time.Millisecond
	reg := &healthStubRegistry{
		engines: map[engine.Name]engine.Engine{
			"primary":   &healthStubEngine{name: "primary", delay: delay},
			"secondary": &healthStubEngine{name: "secondary", delay: delay},
		},
		order: []engine.Name{"primary", "secondary"},
	}
	h := &Health{Registry: reg}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)

	start := time.Now()
	h.Readiness(rec, req)
	elapsed := time.Since(start)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Less(t, elapsed, 700*time.Millisecond,
		"engines must be probed concurrently; elapsed=%s", elapsed)

	var body struct {
		Status  string            `json:"status"`
		Engines map[string]string `json:"engines"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "ready", body.Status)
	require.Equal(t, "ok", body.Engines["primary"])
	require.Equal(t, "ok", body.Engines["secondary"])
}

func TestReadiness_RedactsUpstreamErrorString(t *testing.T) {
	t.Parallel()
	internalErr := "post http://docling.internal:8080/health: connect: connection refused"
	reg := &healthStubRegistry{
		engines: map[engine.Name]engine.Engine{
			"primary": &healthStubEngine{name: "primary", err: errors.New(internalErr)},
		},
		order: []engine.Name{"primary"},
	}
	h := &Health{Registry: reg}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	h.Readiness(rec, req)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	body := rec.Body.String()
	require.NotContains(t, body, "docling.internal", "response leaked backend host")
	require.NotContains(t, body, "connection refused", "response leaked transport detail")

	var parsed struct {
		Status  string            `json:"status"`
		Engines map[string]string `json:"engines"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &parsed))
	require.Equal(t, "not-ready", parsed.Status)
	require.Equal(t, "unhealthy", parsed.Engines["primary"],
		"readyz body must use the redacted literal, never an error message")
}

func TestReadiness_FailureSurfacesAcrossOtherProbes(t *testing.T) {
	t.Parallel()
	reg := &healthStubRegistry{
		engines: map[engine.Name]engine.Engine{
			"primary":   &healthStubEngine{name: "primary", delay: 50 * time.Millisecond},
			"secondary": &healthStubEngine{name: "secondary", delay: 50 * time.Millisecond, err: context.DeadlineExceeded},
		},
		order: []engine.Name{"primary", "secondary"},
	}
	h := &Health{Registry: reg}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	h.Readiness(rec, req)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
}
