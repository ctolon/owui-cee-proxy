package middleware

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ctolon/owui-cee-proxy/internal/observability"
)

// Metrics records Prometheus RED metrics keyed by the engine annotation
// stamped in by the convert handlers.
//
// The `route` label uses chi.RouteContext().RoutePattern() instead of
// the raw URL path so dynamic segments (e.g. /v1/status/poll/{id})
// collapse to a single time-series rather than one per ULID — which
// would explode label cardinality.
func Metrics(m *observability.Metrics) func(http.Handler) http.Handler {
	if m == nil {
		return func(h http.Handler) http.Handler { return h }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}
			start := time.Now()
			next.ServeHTTP(rw, r)

			engineName, _ := engineFrom(r.Context())
			route := routeLabelFromContext(r)
			m.RequestsTotal.WithLabelValues(engineName, route, r.Method, strconv.Itoa(rw.status)).Inc()
			m.RequestDuration.WithLabelValues(engineName, route, r.Method).Observe(time.Since(start).Seconds())
			if rw.status >= 500 {
				m.UpstreamErrors.WithLabelValues(engineName, "5xx").Inc()
			}
			if rw.bytes > 0 {
				m.BodyBytes.WithLabelValues(engineName, "out").Observe(float64(rw.bytes))
			}
			// Inbound body bytes are surfaced by BodyLimit which wraps
			// r.Body in a counter and stashes the counter in context.
			// We only know the final count after the handler returns.
			if n := bodyBytesIn(r.Context()); n > 0 {
				m.BodyBytes.WithLabelValues(engineName, "in").Observe(float64(n))
			}
		})
	}
}

// routeLabelFromContext extracts the chi route pattern (e.g.
// "/v1/status/poll/{id}") so cardinality stays bounded. When the
// router has not matched a pattern (e.g. 404 or middleware running
// before mux), we fall back to "unknown".
func routeLabelFromContext(r *http.Request) string {
	if rctx := chi.RouteContext(r.Context()); rctx != nil {
		if p := rctx.RoutePattern(); p != "" {
			return p
		}
	}
	return "unknown"
}
