package middleware

import (
	"net/http"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// OTel wraps the handler with OpenTelemetry HTTP server instrumentation.
// Disabled by passing service="".
func OTel(service string) func(http.Handler) http.Handler {
	if service == "" {
		return func(h http.Handler) http.Handler { return h }
	}
	return func(next http.Handler) http.Handler {
		return otelhttp.NewHandler(next, service)
	}
}
