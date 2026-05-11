// File-level note: this is the outbound observability layer. Every
// engine-bound HTTP request flows through InstrumentedTransport, which
// emits a debug `outbound_engine_request` event and bumps the
// engine_upstream_status_total counter on the response. The auth-stamping
// counter is recorded separately by authutil.Apply BEFORE the request
// leaves; together the two surfaces let operators distinguish "we
// attached the header" from "the backend accepted it".

package httpclient

import (
	"bytes"
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/rs/zerolog"

	mw "github.com/ctolon/owui-cee-proxy/internal/transport/http/middleware"
)

// bodySnippetBufPool recycles the bytes.Buffer used by
// InstrumentedTransport when BodyLogBytes>0. The buffer is reset and
// returned to the pool after the log line is emitted; this keeps the
// allocation off the hot path when debug body logging is on (it's
// off by default so this rarely matters in practice).
var bodySnippetBufPool = sync.Pool{
	New: func() any { return &bytes.Buffer{} },
}

// UpstreamStatusRecorder is the metrics seam for outbound HTTP status
// codes. The production wiring binds it to the
// EngineUpstreamStatus Prometheus counter; tests pass a fake.
type UpstreamStatusRecorder interface {
	RecordUpstreamStatus(engine string, status int)
}

// nopUpstreamRecorder implements UpstreamStatusRecorder with zero side
// effects. Used when no metric is wired (tests, contract suite).
type nopUpstreamRecorder struct{}

func (nopUpstreamRecorder) RecordUpstreamStatus(string, int) {}

// InstrumentedTransport wraps an http.RoundTripper to log every
// outbound request at debug level and to report its response status
// to UpstreamStatusRecorder. Field summary:
//
//   - engine_name        : engines.<name> key the request is bound to
//   - engine_url         : engines.<name>.url (the base, not the full URL)
//   - auth_header        : configured header (empty when no auth)
//   - auth_scheme        : "raw" | "bearer" | ""
//   - auth_key_present   : true when a non-empty key was attached
//   - auth_key_len       : length of the key as it appeared on the wire
//     (Bearer prefix excluded). 0 when no key.
//   - body_snippet_b64   : first BodyLogBytes of the request body,
//     base64-encoded. Empty when BodyLogBytes=0.
//
// Header values are NEVER logged. The proxy's contract is "I show you
// that I tried to send auth, never what I sent."
type InstrumentedTransport struct {
	Next         http.RoundTripper
	Logger       zerolog.Logger
	Recorder     UpstreamStatusRecorder
	EngineName   string
	EngineURL    string
	AuthHeader   string
	AuthScheme   string
	BodyLogBytes int
}

func (t *InstrumentedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	authValuePresent := false
	authValueLen := 0
	if t.AuthHeader != "" {
		v := req.Header.Get(t.AuthHeader)
		if v != "" {
			authValuePresent = true
			// Strip the "Bearer " prefix from the reported length so
			// operators can compare against the expected key size,
			// not "key + 7".
			trimmed := strings.TrimPrefix(v, "Bearer ")
			authValueLen = len(trimmed)
		}
	}

	var bodySnippet string
	if t.BodyLogBytes > 0 && req.Body != nil && req.Body != http.NoBody {
		buf := bodySnippetBufPool.Get().(*bytes.Buffer)
		buf.Reset()
		// Read at most BodyLogBytes and rewind by replacing Body with
		// a MultiReader. NOTE: this forces the buffered prefix into
		// memory; operators MUST keep BodyLogBytes small (default 0,
		// suggested 1-4 KiB for debugging).
		_, _ = io.CopyN(buf, req.Body, int64(t.BodyLogBytes))
		bodySnippet = base64.StdEncoding.EncodeToString(buf.Bytes())
		// We must COPY the bytes out of buf before returning it to
		// the pool — the MultiReader keeps a reference until the
		// outbound request is fully drained, and another goroutine
		// may have already pulled the same buffer back out.
		snapshot := make([]byte, buf.Len())
		copy(snapshot, buf.Bytes())
		bodySnippetBufPool.Put(buf)

		original := req.Body
		req.Body = struct {
			io.Reader
			io.Closer
		}{
			Reader: io.MultiReader(bytes.NewReader(snapshot), original),
			Closer: original,
		}
	}

	// Emit the per-request debug log unless the caller has marked
	// the context silent (health probes do this when
	// observability.log.silence_engine_health=true). The status
	// counter below still fires regardless of suppression so
	// dashboards remain accurate.
	if !mw.IsLogSilenced(req.Context()) {
		t.Logger.Debug().
			Str("event", "outbound_engine_request").
			Str("engine", t.EngineName).
			Str("engine_url", t.EngineURL).
			Str("method", req.Method).
			Str("path", req.URL.Path).
			Str("mime_type", req.Header.Get("Content-Type")).
			Str("auth_header", t.AuthHeader).
			Str("auth_scheme", t.AuthScheme).
			Bool("auth_key_present", authValuePresent).
			Int("auth_key_len", authValueLen).
			Str("body_snippet_b64", bodySnippet).
			Msg("outbound engine request")
	}

	next := t.Next
	if next == nil {
		next = http.DefaultTransport
	}
	resp, err := next.RoundTrip(req)
	if err != nil {
		t.recorder().RecordUpstreamStatus(t.EngineName, 0)
		return nil, err
	}
	t.recorder().RecordUpstreamStatus(t.EngineName, resp.StatusCode)
	return resp, nil
}

func (t *InstrumentedTransport) recorder() UpstreamStatusRecorder {
	if t.Recorder == nil {
		return nopUpstreamRecorder{}
	}
	return t.Recorder
}

// Instrument wraps c's underlying transport with InstrumentedTransport.
// Returns the same Client so the call composes after New() at the
// composition root: `httpclient.New(cfg).Instrument(opts)`. Idempotent
// on repeat calls (subsequent calls re-wrap, which is harmless but
// wasteful — composition root should call exactly once per client).
func (c *Client) Instrument(opts InstrumentOptions) *Client {
	base := c.HTTP.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	c.HTTP.Transport = &InstrumentedTransport{
		Next:         base,
		Logger:       opts.Logger,
		Recorder:     opts.Recorder,
		EngineName:   opts.EngineName,
		EngineURL:    c.BaseURL,
		AuthHeader:   opts.AuthHeader,
		AuthScheme:   opts.AuthScheme,
		BodyLogBytes: opts.BodyLogBytes,
	}
	return c
}

// InstrumentOptions bundles the per-engine configuration the
// InstrumentedTransport needs. The composition root assembles one of
// these per engine (it knows the engine name and config).
type InstrumentOptions struct {
	Logger       zerolog.Logger
	Recorder     UpstreamStatusRecorder
	EngineName   string
	AuthHeader   string
	AuthScheme   string
	BodyLogBytes int
}
