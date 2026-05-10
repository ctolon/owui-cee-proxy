package middleware

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

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
