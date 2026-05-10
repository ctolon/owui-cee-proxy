package middleware

import (
	"net/http"
	"strconv"
	"time"

	"github.com/ctolon/owui-cee-proxy/internal/observability"
)

// Metrics records Prometheus RED metrics keyed by the engine annotation
// stamped in by the convert handlers.
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
			route := r.URL.Path
			m.RequestsTotal.WithLabelValues(engineName, route, r.Method, strconv.Itoa(rw.status)).Inc()
			m.RequestDuration.WithLabelValues(engineName, route, r.Method).Observe(time.Since(start).Seconds())
			if rw.status >= 500 {
				m.UpstreamErrors.WithLabelValues(engineName, "5xx").Inc()
			}
			if rw.bytes > 0 {
				m.BodyBytes.WithLabelValues(engineName, "out").Observe(float64(rw.bytes))
			}
		})
	}
}
