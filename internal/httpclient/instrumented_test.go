package httpclient

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	mw "github.com/ctolon/owui-cee-proxy/internal/transport/http/middleware"
)

type recordingRecorder struct {
	calls []struct {
		engine string
		status int
	}
}

func (r *recordingRecorder) RecordUpstreamStatus(engine string, status int) {
	r.calls = append(r.calls, struct {
		engine string
		status int
	}{engine, status})
}

// errTransport simulates a transport-level error so we can verify the
// instrumented transport records status=0 in that case.
type errTransport struct{ err error }

func (e errTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, e.err }

func TestInstrumentedTransport_RecordsResponseStatus(t *testing.T) {
	t.Parallel()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(401)
	}))
	defer backend.Close()

	logBuf := &bytes.Buffer{}
	logger := zerolog.New(logBuf).Level(zerolog.DebugLevel)
	rec := &recordingRecorder{}

	itrans := &InstrumentedTransport{
		Next:       http.DefaultTransport,
		Logger:     logger,
		Recorder:   rec,
		EngineName: "main-docling",
		EngineURL:  backend.URL,
		AuthHeader: "X-Api-Key",
		AuthScheme: "raw",
	}
	req, err := http.NewRequest("POST", backend.URL+"/v1/convert/file", strings.NewReader("ignored"))
	require.NoError(t, err)
	req.Header.Set("X-Api-Key", "the-key")

	resp, err := itrans.RoundTrip(req)
	require.NoError(t, err)
	require.NotNil(t, resp)
	resp.Body.Close()

	require.Len(t, rec.calls, 1)
	require.Equal(t, "main-docling", rec.calls[0].engine)
	require.Equal(t, 401, rec.calls[0].status)

	// Debug log line must declare auth_key_present=true and a key
	// length matching what we set (no Bearer prefix to strip here).
	require.Contains(t, logBuf.String(), `"event":"outbound_engine_request"`)
	require.Contains(t, logBuf.String(), `"auth_key_present":true`)
	require.Contains(t, logBuf.String(), `"auth_key_len":7`)
	require.Contains(t, logBuf.String(), `"engine":"main-docling"`)
	// The actual key value MUST NEVER leak.
	require.NotContains(t, logBuf.String(), "the-key")
}

func TestInstrumentedTransport_TransportErrorRecordsZeroStatus(t *testing.T) {
	t.Parallel()
	rec := &recordingRecorder{}
	itrans := &InstrumentedTransport{
		Next:       errTransport{err: errors.New("dial: connection refused")},
		Logger:     zerolog.New(io.Discard),
		Recorder:   rec,
		EngineName: "broken",
	}
	req, err := http.NewRequest("GET", "http://nowhere/health", nil)
	require.NoError(t, err)

	resp, err := itrans.RoundTrip(req)
	require.Error(t, err)
	require.Nil(t, resp)
	require.Len(t, rec.calls, 1)
	require.Equal(t, 0, rec.calls[0].status, "transport error must record status=0")
}

func TestInstrumentedTransport_BodySnippetCapturedAndRestored(t *testing.T) {
	t.Parallel()
	var seenBody string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seenBody = string(b)
	}))
	defer backend.Close()

	logBuf := &bytes.Buffer{}
	itrans := &InstrumentedTransport{
		Next:         http.DefaultTransport,
		Logger:       zerolog.New(logBuf).Level(zerolog.DebugLevel),
		Recorder:     &recordingRecorder{},
		EngineName:   "main",
		EngineURL:    backend.URL,
		BodyLogBytes: 8,
	}
	req, err := http.NewRequest("POST", backend.URL+"/echo", strings.NewReader("hello-world-12345"))
	require.NoError(t, err)

	resp, err := itrans.RoundTrip(req)
	require.NoError(t, err)
	resp.Body.Close()

	// Backend must have received the FULL body, not just the snippet.
	require.Equal(t, "hello-world-12345", seenBody)
	// Log contains a base64 snippet of the first 8 bytes ("hello-wo").
	require.Contains(t, logBuf.String(), `"body_snippet_b64":"`)
	require.Contains(t, logBuf.String(), `aGVsbG8td28=`) // base64("hello-wo")
}

// TestInstrumentedTransport_SilenceLogCtxMutesDebugLine pins the
// readiness-probe path: when the ctx is marked silent, the verbose
// outbound debug log is dropped but the status counter still fires.
func TestInstrumentedTransport_SilenceLogCtxMutesDebugLine(t *testing.T) {
	t.Parallel()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))
	defer backend.Close()

	logBuf := &bytes.Buffer{}
	rec := &recordingRecorder{}
	itrans := &InstrumentedTransport{
		Next:       http.DefaultTransport,
		Logger:     zerolog.New(logBuf).Level(zerolog.DebugLevel),
		Recorder:   rec,
		EngineName: "main",
		EngineURL:  backend.URL,
	}
	req, err := http.NewRequest("GET", backend.URL+"/health", nil)
	require.NoError(t, err)
	// Mark the request silent — as the readiness handler does
	// when observability.log.silence_engine_health=true.
	req = req.WithContext(mw.WithSilenceLog(req.Context()))

	resp, err := itrans.RoundTrip(req)
	require.NoError(t, err)
	resp.Body.Close()

	require.Empty(t, logBuf.String(), "silenced ctx must drop the debug log line")
	require.Len(t, rec.calls, 1, "metric still fires regardless of log suppression")
	require.Equal(t, 200, rec.calls[0].status)
}

func TestInstrumentedTransport_BearerPrefixStrippedFromKeyLen(t *testing.T) {
	t.Parallel()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer backend.Close()

	logBuf := &bytes.Buffer{}
	itrans := &InstrumentedTransport{
		Next:       http.DefaultTransport,
		Logger:     zerolog.New(logBuf).Level(zerolog.DebugLevel),
		Recorder:   &recordingRecorder{},
		EngineName: "kreuzberg",
		EngineURL:  backend.URL,
		AuthHeader: "Authorization",
		AuthScheme: "bearer",
	}
	req, err := http.NewRequest("GET", backend.URL+"/health", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer my-token") // raw len = 8

	resp, err := itrans.RoundTrip(req)
	require.NoError(t, err)
	resp.Body.Close()

	require.Contains(t, logBuf.String(), `"auth_key_len":8`,
		"Bearer prefix must be excluded from the reported key length")
}
