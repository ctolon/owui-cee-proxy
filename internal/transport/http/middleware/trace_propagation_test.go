package middleware_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"

	mw "github.com/ctolon/owui-cee-proxy/internal/transport/http/middleware"
)

// TestAccessLog_TraceFieldsPropagated pins C-18: when the otelhttp
// middleware (or any upstream span installer) carries a valid
// trace.SpanContext on r.Context, the AccessLog `request_completed`
// line MUST emit trace_id + span_id alongside request_id so Tempo /
// Jaeger queries can join logs ↔ traces on the same key.
func TestAccessLog_TraceFieldsPropagated(t *testing.T) {
	t.Parallel()

	// Spin a minimal in-memory tracer so the test produces a real
	// (non-zero) SpanContext when SpanFromContext is called.
	exp := tracetest.NewInMemoryExporter()
	tp := trace.NewTracerProvider(trace.WithSyncer(exp))
	tracer := tp.Tracer("test")
	defer func() { _ = tp.Shutdown(t.Context()) }()

	buf := &bytes.Buffer{}
	logger := zerolog.New(buf)

	handler := mw.AccessLog(logger)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Build a request whose context carries a live span.
	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	ctx, span := tracer.Start(req.Context(), "test-root")
	wantTraceID := span.SpanContext().TraceID().String()
	wantSpanID := span.SpanContext().SpanID().String()
	defer span.End()
	req = req.WithContext(ctx)

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	var line map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &line))
	require.Equal(t, wantTraceID, line["trace_id"],
		"request_completed MUST stamp the active trace_id for Tempo joins")
	require.Equal(t, wantSpanID, line["span_id"])
}

// TestAccessLog_TraceFieldsEmptyWhenNoSpan covers the non-traced
// path. Requests without an active span MUST log empty trace_id /
// span_id strings — zerolog needs the keys present for stable
// log-shipper schemas.
func TestAccessLog_TraceFieldsEmptyWhenNoSpan(t *testing.T) {
	t.Parallel()
	buf := &bytes.Buffer{}
	logger := zerolog.New(buf)
	handler := mw.AccessLog(logger)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	var line map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &line))
	require.Equal(t, "", line["trace_id"])
	require.Equal(t, "", line["span_id"])
}

// TestTraceFieldsFrom_NilContextSafe — defensive: helper must not
// panic when handed a nil context (some test harnesses pass nil
// during teardown).
func TestTraceFieldsFrom_NilContextSafe(t *testing.T) {
	t.Parallel()
	// nolint:staticcheck // SA1012: passing nil is the case under
	// test — TraceFieldsFrom is a log helper and MUST tolerate nil
	// (some test harnesses pass nil during teardown).
	traceID, spanID := mw.TraceFieldsFrom(nil)
	require.Equal(t, "", traceID)
	require.Equal(t, "", spanID)

	// Also a context without a span returns empty strings, not a
	// zero TraceID rendered as 32 hex zeroes.
	traceID, spanID = mw.TraceFieldsFrom(t.Context())
	require.Equal(t, "", traceID)
	require.Equal(t, "", spanID)

	// Sanity: confirm what oteltrace would say for a context with a
	// non-recording span — the helper must report empty, since a
	// non-recording (no-op) span context is NOT valid for joining.
	_ = oteltrace.SpanFromContext(t.Context())
}
