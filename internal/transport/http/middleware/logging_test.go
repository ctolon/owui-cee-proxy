package middleware

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// hijackableRecorder is a httptest.ResponseRecorder + http.Hijacker
// stub. We intercept Hijack() to record that the wrapper delegated
// the call instead of failing.
type hijackableRecorder struct {
	*httptest.ResponseRecorder
	hijackedConn net.Conn
	hijackedRW   *bufio.ReadWriter
	hijackErr    error
	called       bool
}

func (h *hijackableRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h.called = true
	return h.hijackedConn, h.hijackedRW, h.hijackErr
}

func TestResponseWriter_HijackDelegatesWhenSupported(t *testing.T) {
	t.Parallel()
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	rw := &hijackableRecorder{
		ResponseRecorder: httptest.NewRecorder(),
		hijackedConn:     c1,
		hijackedRW:       bufio.NewReadWriter(bufio.NewReader(c1), bufio.NewWriter(c1)),
	}
	wrapped := &responseWriter{ResponseWriter: rw, status: http.StatusOK}

	conn, brw, err := wrapped.Hijack()
	require.NoError(t, err)
	require.True(t, rw.called, "wrapper must delegate Hijack to underlying writer")
	require.Same(t, c1, conn)
	require.NotNil(t, brw)
}

func TestResponseWriter_HijackErrorsWhenUnsupported(t *testing.T) {
	t.Parallel()
	// httptest.ResponseRecorder does NOT implement http.Hijacker, so
	// the wrapper should surface a clear error instead of panicking.
	rw := &responseWriter{ResponseWriter: httptest.NewRecorder(), status: http.StatusOK}
	_, _, err := rw.Hijack()
	require.Error(t, err)
}

func TestResponseWriter_HijackPropagatesUnderlyingError(t *testing.T) {
	t.Parallel()
	want := errors.New("forced hijack failure")
	rw := &hijackableRecorder{
		ResponseRecorder: httptest.NewRecorder(),
		hijackErr:        want,
	}
	wrapped := &responseWriter{ResponseWriter: rw, status: http.StatusOK}

	_, _, err := wrapped.Hijack()
	require.ErrorIs(t, err, want)
}

func TestResponseWriter_WriteHeaderOnce(t *testing.T) {
	t.Parallel()
	// sync.Once guarantees the underlying WriteHeader is called only
	// once even if the handler invokes it multiple times.
	rec := httptest.NewRecorder()
	rw := &responseWriter{ResponseWriter: rec, status: http.StatusOK}
	rw.WriteHeader(http.StatusTeapot)
	rw.WriteHeader(http.StatusInternalServerError)
	require.Equal(t, http.StatusTeapot, rw.status)
	require.Equal(t, http.StatusTeapot, rec.Code)
}

func TestResponseWriter_PushReturnsErrNotSupportedWhenAbsent(t *testing.T) {
	t.Parallel()
	rw := &responseWriter{ResponseWriter: httptest.NewRecorder(), status: http.StatusOK}
	require.Equal(t, http.ErrNotSupported, rw.Push("/x", nil))
}

// TestAccessLog_EmitsStageTimings exercises the full middleware against
// a handler that pretends to do upstream work (StartUpstream → sleep →
// EndUpstream) and asserts that the resulting log line carries the
// three timing fields the operator needs: total, time_to_first_byte,
// upstream. Without this test a regression that forgets to call
// recordTTFB in WriteHeader would silently re-zero the TTFB field.
func TestAccessLog_EmitsStageTimings(t *testing.T) {
	t.Parallel()

	buf := &bytes.Buffer{}
	logger := zerolog.New(buf).Level(zerolog.InfoLevel)
	handler := AccessLog(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WithEngine(r.Context(), "docling", "http://docling:5001")
		WithFilename(r.Context(), "test.pdf")
		WithFileCount(r.Context(), 1)
		StartUpstream(r.Context())
		time.Sleep(10 * time.Millisecond)
		EndUpstream(r.Context(), 200)
		w.WriteHeader(200)
		_, _ = w.Write([]byte("ok"))
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/convert/file", nil)
	handler.ServeHTTP(rec, req)

	var line map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &line))

	require.Equal(t, "request_completed", line["event"])
	require.Equal(t, "docling", line["engine"])
	require.Equal(t, "http://docling:5001", line["engine_url"])
	require.Equal(t, "test.pdf", line["filename"])
	require.Equal(t, float64(200), line["upstream_status"])

	total := line["total_ms"].(float64)
	upstream := line["upstream_ms"].(float64)
	ttfb := line["time_to_first_byte_ms"].(float64)

	require.GreaterOrEqual(t, upstream, float64(10), "upstream sleep must register")
	require.GreaterOrEqual(t, total, upstream, "total cannot be less than upstream")
	require.GreaterOrEqual(t, ttfb, upstream, "TTFB happens AFTER upstream returns")
}

// TestAccessLog_UpstreamStatusZeroWhenNoEngineCall pins the "no
// upstream call" path: a request rejected at the middleware layer
// (e.g., 401 from APIKey middleware) should still log a clean
// upstream_status:0 + upstream_ms:0 instead of stale data.
func TestAccessLog_UpstreamStatusZeroWhenNoEngineCall(t *testing.T) {
	t.Parallel()
	buf := &bytes.Buffer{}
	logger := zerolog.New(buf).Level(zerolog.InfoLevel)
	handler := AccessLog(logger)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(401)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/convert/file", nil)
	handler.ServeHTTP(rec, req)

	var line map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &line))
	require.Equal(t, float64(0), line["upstream_status"])
	require.Equal(t, float64(0), line["upstream_ms"])
}

// TestUpstreamDurationFrom_NilCtxReturnsZero defends against nil
// dereference for handlers that build upstream durations without an
// AccessLog wrapper in tests.
func TestUpstreamDurationFrom_NilCtxReturnsZero(t *testing.T) {
	t.Parallel()
	require.Equal(t, time.Duration(0), UpstreamDurationFrom(context.Background()))
	require.Equal(t, 0, UpstreamStatusFrom(context.Background()))
}

// TestAccessLog_SilencePathsDropsLogLine pins the operator knob:
// requests to a path in the silence list MUST NOT produce a
// request_completed line. Without this test a regression in the
// silenceSet build (e.g., trimming "/" suffix) would silently
// re-flood the readiness logs.
func TestAccessLog_SilencePathsDropsLogLine(t *testing.T) {
	t.Parallel()
	buf := &bytes.Buffer{}
	logger := zerolog.New(buf).Level(zerolog.InfoLevel)
	handler := AccessLog(logger, "/healthz", "/readyz", "/metrics")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))

	for _, p := range []string{"/healthz", "/readyz", "/metrics"} {
		buf.Reset()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", p, nil)
		handler.ServeHTTP(rec, req)
		require.Empty(t, buf.String(), "path %s should not produce a log line", p)
	}

	// A non-silenced path still emits.
	buf.Reset()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/convert/file", nil)
	handler.ServeHTTP(rec, req)
	require.Contains(t, buf.String(), "request_completed")
}

// TestAccessLog_WithSilenceLogCtxDropsEvenNonListedPaths covers the
// programmatic suppression path (readiness handler wraps ctx for
// engine-probe sub-requests when SilenceEngineHealth=true).
func TestAccessLog_WithSilenceLogCtxDropsEvenNonListedPaths(t *testing.T) {
	t.Parallel()
	buf := &bytes.Buffer{}
	logger := zerolog.New(buf).Level(zerolog.InfoLevel)
	handler := AccessLog(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Handler opts the ctx into silence — equivalent to the
		// readiness handler marking its sub-flows quiet.
		*r = *r.WithContext(WithSilenceLog(r.Context()))
		w.WriteHeader(200)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/convert/file", nil)
	handler.ServeHTTP(rec, req)
	require.Empty(t, buf.String(), "ctx-silenced request must not log")
}

// TestIsLogSilenced_NilCtx defends a defensive caller path: the
// InstrumentedTransport may be invoked with a nil-derived ctx in
// rare test setups.
func TestIsLogSilenced_NilCtx(t *testing.T) {
	t.Parallel()
	// staticcheck flags nil-Context arguments at compile time; the
	// whole point of THIS test is to prove IsLogSilenced doesn't
	// panic on the nil-ctx path. nolint surfaces the intent.
	require.False(t, IsLogSilenced(nil)) //nolint:staticcheck // SA1012: defensive nil-ctx test.
	require.False(t, IsLogSilenced(context.Background()))
	require.True(t, IsLogSilenced(WithSilenceLog(context.Background())))
}
