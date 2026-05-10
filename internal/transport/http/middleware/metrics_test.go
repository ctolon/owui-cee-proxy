package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"
)

func TestRouteLabel_NoChiContextReturnsUnknown(t *testing.T) {
	t.Parallel()
	// Plain request with no chi route context attached. The metrics
	// middleware should fall back to the literal "unknown" label
	// rather than emitting the raw path (which would explode label
	// cardinality on dynamic IDs).
	req := httptest.NewRequest(http.MethodGet, "/v1/status/poll/01HM-some-ulid", nil)
	got := routeLabelFromContext(req)
	require.Equal(t, "unknown", got)
}

func TestRouteLabel_EmptyPatternFallsBackToUnknown(t *testing.T) {
	t.Parallel()
	// chi installs a RouteContext on every request, but
	// RoutePattern() is "" until the mux finds a matching route.
	// Until it does (e.g. because we're inside a global pre-mux
	// middleware), the label must collapse to "unknown".
	rctx := chi.NewRouteContext()
	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	got := routeLabelFromContext(req)
	require.Equal(t, "unknown", got)
}

func TestRouteLabel_PatternMatched(t *testing.T) {
	t.Parallel()
	// Drive a real chi mux so we exercise the same code path the
	// production middleware sees: chi populates RoutePattern() once
	// the matched route is resolved, and the label should be the
	// pattern string ("/v1/status/poll/{id}"), not the raw URL.
	var got string
	r := chi.NewRouter()
	r.Get("/v1/status/poll/{id}", func(w http.ResponseWriter, req *http.Request) {
		got = routeLabelFromContext(req)
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/status/poll/01HM-some-ulid", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "/v1/status/poll/{id}", got)
}
