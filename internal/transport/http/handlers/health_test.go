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
func (s *healthStubEngine) Convert(ctx context.Context, _ *engine.ConvertRequest) (*engine.ConvertResponse, error) {
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
func (r *healthStubRegistry) Pick(_ string) engine.Engine {
	return r.engines[r.order[0]]
}
func (r *healthStubRegistry) Names() []engine.Name { return r.order }

func TestReadiness_RunsHealthChecksConcurrently(t *testing.T) {
	t.Parallel()
	// Two stub engines, each sleeping ~400ms in Health(). If the
	// handler runs them sequentially the total wall time is ~800ms
	// + overhead. Concurrent execution should finish in ~400ms +
	// overhead. We assert clearly under 700ms — generous to keep
	// the test stable on slow CI but tight enough to catch a
	// regression to sequential.
	delay := 400 * time.Millisecond
	reg := &healthStubRegistry{
		engines: map[engine.Name]engine.Engine{
			engine.Docling: &healthStubEngine{name: engine.Docling, delay: delay},
			engine.Tika:    &healthStubEngine{name: engine.Tika, delay: delay},
		},
		order: []engine.Name{engine.Docling, engine.Tika},
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
	require.Equal(t, "ok", body.Engines["docling"])
	require.Equal(t, "ok", body.Engines["tika"])
}

// M4 — readyz must not echo upstream error strings into the response
// body. The response should carry the literal "unhealthy" instead, so
// callers cannot fingerprint backend hostnames, ports, or breaker
// state. The full error text still reaches the configured logger.
func TestReadiness_RedactsUpstreamErrorString(t *testing.T) {
	t.Parallel()
	internal := "post http://docling.internal:8080/health: connect: connection refused"
	reg := &healthStubRegistry{
		engines: map[engine.Name]engine.Engine{
			engine.Docling: &healthStubEngine{name: engine.Docling, err: errors.New(internal)},
		},
		order: []engine.Name{engine.Docling},
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
	require.Equal(t, "unhealthy", parsed.Engines["docling"],
		"readyz body must use the redacted literal, never an error message")
}

func TestReadiness_FailureSurfacesAcrossOtherProbes(t *testing.T) {
	t.Parallel()
	// One engine fails, another succeeds. The successful one's
	// status should still be reported, and overall should flip to
	// 503. This protects the parallel-probe contract: a failure
	// must not abort still-running probes.
	reg := &healthStubRegistry{
		engines: map[engine.Name]engine.Engine{
			engine.Docling: &healthStubEngine{name: engine.Docling, delay: 50 * time.Millisecond},
			engine.Tika:    &healthStubEngine{name: engine.Tika, delay: 50 * time.Millisecond, err: context.DeadlineExceeded},
		},
		order: []engine.Name{engine.Docling, engine.Tika},
	}
	h := &Health{Registry: reg}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	h.Readiness(rec, req)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
}
