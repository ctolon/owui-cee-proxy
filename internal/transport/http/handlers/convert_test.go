package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/ctolon/owui-cee-proxy/internal/engine"
	mw "github.com/ctolon/owui-cee-proxy/internal/transport/http/middleware"
)

type recordingEngine struct {
	name engine.Name
	hits *atomic.Int32
}

func (r *recordingEngine) Name() engine.Name { return r.name }
func (r *recordingEngine) URL() string       { return "http://" + string(r.name) + ".test" }
func (r *recordingEngine) Capabilities() engine.EngineCapabilities {
	return engine.EngineCapabilities{
		Facades:     []engine.Facade{engine.FacadeDocling},
		HTTPSources: true,
	}
}

func (r *recordingEngine) Convert(_ context.Context, _ *engine.ConvertRequest) (*engine.ConvertResponse, error) {
	r.hits.Add(1)
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	return &engine.ConvertResponse{
		StatusCode: http.StatusOK,
		Headers:    h,
		Body:       io.NopCloser(strings.NewReader(`{"status":"success"}`)),
	}, nil
}

func (r *recordingEngine) Health(_ context.Context) error { return nil }

func TestConvert_RoutesByMIME(t *testing.T) {
	t.Parallel()

	defaultHits, pdfHits, imgHits := &atomic.Int32{}, &atomic.Int32{}, &atomic.Int32{}
	d := &recordingEngine{name: "default-eng", hits: defaultHits}
	pdf := &recordingEngine{name: "pdf-eng", hits: pdfHits}
	img := &recordingEngine{name: "img-eng", hits: imgHits}

	registry, err := engine.NewRegistry(map[engine.Name]engine.RegistryEntry{
		"default-eng": {Engine: d},
		"pdf-eng":     {Engine: pdf, MimeTypes: []string{"application/pdf"}},
		"img-eng":     {Engine: img, MimeTypes: []string{"image/*"}},
	}, "default-eng", "")
	require.NoError(t, err)

	h := &Convert{Registry: registry}
	srv := httptest.NewServer(http.HandlerFunc(h.File))
	defer srv.Close()

	cases := []struct {
		mime    string
		want    *atomic.Int32
		wantOne int32
	}{
		{"application/pdf", pdfHits, 1},
		{"image/png", imgHits, 1},
		{"application/octet-stream", defaultHits, 1},
		{"text/plain", defaultHits, 2},
	}

	for _, c := range cases {
		body, contentType := buildMultipart(t, "x", c.mime)
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, body)
		require.NoError(t, err)
		req.Header.Set("Content-Type", contentType)
		resp, err := srv.Client().Do(req)
		require.NoError(t, err, "mime=%s", c.mime)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.EqualValues(t, c.wantOne, c.want.Load(), "mime=%s wrong engine", c.mime)
	}
}

func TestConvert_SourceModeUsesDefault(t *testing.T) {
	t.Parallel()
	defaultHits, otherHits := &atomic.Int32{}, &atomic.Int32{}
	d := &recordingEngine{name: "default-eng", hits: defaultHits}
	o := &recordingEngine{name: "other-eng", hits: otherHits}

	registry, err := engine.NewRegistry(map[engine.Name]engine.RegistryEntry{
		"default-eng": {Engine: d},
		"other-eng":   {Engine: o, MimeTypes: []string{"application/pdf"}},
	}, "default-eng", "")
	require.NoError(t, err)

	h := &Convert{Registry: registry}
	srv := httptest.NewServer(http.HandlerFunc(h.Source))
	defer srv.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL,
		strings.NewReader(`{"http_sources":[{"url":"https://1.1.1.1/x.pdf"}]}`))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Zero(t, otherHits.Load(), "source mode must NOT route by mime")
	require.EqualValues(t, 1, defaultHits.Load())
}

type failingEngine struct {
	name engine.Name
	err  error
}

func (f *failingEngine) Name() engine.Name { return f.name }
func (f *failingEngine) URL() string       { return "http://" + string(f.name) + ".test" }
func (f *failingEngine) Capabilities() engine.EngineCapabilities {
	return engine.EngineCapabilities{Facades: []engine.Facade{engine.FacadeDocling}, HTTPSources: true}
}

func (f *failingEngine) Convert(_ context.Context, _ *engine.ConvertRequest) (*engine.ConvertResponse, error) {
	return nil, f.err
}

func (f *failingEngine) Health(_ context.Context) error { return nil }

// TestConvert_EngineErrorIsRedacted (M5) — engine errors that contain
// internal URLs / hostnames must not leak into the caller response.
func TestConvert_EngineErrorIsRedacted(t *testing.T) {
	t.Parallel()

	internal := "http://docling-serve.internal:8080/v1/convert/file"
	fe := &failingEngine{
		name: "primary",
		err:  errors.New("post " + internal + ": connect: connection refused"),
	}
	registry, err := engine.NewRegistry(map[engine.Name]engine.RegistryEntry{
		"primary": {Engine: fe},
	}, "primary", "")
	require.NoError(t, err)

	h := &Convert{Registry: registry}
	srv := httptest.NewServer(http.HandlerFunc(h.File))
	defer srv.Close()

	body, contentType := buildMultipart(t, "x", "application/pdf")
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, body)
	require.NoError(t, err)
	req.Header.Set("Content-Type", contentType)
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadGateway, resp.StatusCode)

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	out := string(raw)

	require.NotContains(t, out, internal, "response leaked backend URL")
	require.NotContains(t, out, "connection refused", "response leaked transport detail")
	require.Contains(t, out, "request_id=", "response should reference request_id for correlation")

	var parsed struct {
		Status string              `json:"status"`
		Errors []map[string]string `json:"errors"`
	}
	require.NoError(t, json.Unmarshal(raw, &parsed))
	require.Equal(t, "failure", parsed.Status)
	require.Len(t, parsed.Errors, 1)
	require.Contains(t, parsed.Errors[0]["message"], "engine primary failed")
}

func TestConvert_ResolveURLDefaultsWhenNil(t *testing.T) {
	t.Parallel()
	d := &recordingEngine{name: "primary", hits: &atomic.Int32{}}
	registry, err := engine.NewRegistry(map[engine.Name]engine.RegistryEntry{
		"primary": {Engine: d},
	}, "primary", "")
	require.NoError(t, err)

	h := &Convert{Registry: registry, ResolveURL: nil}
	srv := httptest.NewServer(http.HandlerFunc(h.Source))
	defer srv.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL,
		strings.NewReader(`{"http_sources":[{"url":"http://127.0.0.1/x.pdf"}]}`))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Zero(t, d.hits.Load(), "engine must not be called when SSRF policy rejects")
}

func TestConvert_ResolveURLOverride(t *testing.T) {
	t.Parallel()
	d := &recordingEngine{name: "primary", hits: &atomic.Int32{}}
	registry, err := engine.NewRegistry(map[engine.Name]engine.RegistryEntry{
		"primary": {Engine: d},
	}, "primary", "")
	require.NoError(t, err)

	calls := 0
	h := &Convert{
		Registry: registry,
		ResolveURL: func(raw string) (*ResolvedURL, error) {
			calls++
			return &ResolvedURL{}, nil
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(h.Source))
	defer srv.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL,
		strings.NewReader(`{"http_sources":[{"url":"http://10.0.0.1/x.pdf"}]}`))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, 1, calls, "custom resolver must be invoked")
	require.EqualValues(t, 1, d.hits.Load(), "engine must run when custom resolver passes")
}

// TestConvert_FormFieldExceedsCapRejected pins the C-6 DoS defence:
// a non-file form-field part larger than maxFormFieldBytes (1 MiB)
// MUST fail the request with 400. Without the cap a hostile client
// could submit a 499 MiB form field under the 500 MiB BodyLimit and
// force the proxy to materialise it as a string.
func TestConvert_FormFieldExceedsCapRejected(t *testing.T) {
	t.Parallel()
	d := &recordingEngine{name: "default-eng", hits: &atomic.Int32{}}
	registry, err := engine.NewRegistry(map[engine.Name]engine.RegistryEntry{
		"default-eng": {Engine: d},
	}, "default-eng", engine.StrategyMimeThenExt)
	require.NoError(t, err)

	h := &Convert{Registry: registry}
	srv := httptest.NewServer(http.HandlerFunc(h.File))
	defer srv.Close()

	// Build a multipart with ONE file part (small) + ONE form-field
	// part that overflows the 1 MiB cap by 1 byte.
	buf := &bytes.Buffer{}
	mw := multipart.NewWriter(buf)
	require.NoError(t, mw.WriteField("dummy", strings.Repeat("A", (1<<20)+1)))
	fw, err := mw.CreateFormFile("files", "x.txt")
	require.NoError(t, err)
	_, _ = fw.Write([]byte("hi"))
	require.NoError(t, mw.Close())

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, buf)
	require.NoError(t, err)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusBadRequest, resp.StatusCode,
		"oversized form field MUST 400 before reaching the engine")
}

func buildMultipart(t *testing.T, body, contentType string) (io.Reader, string) {
	t.Helper()
	return buildMultipartNamed(t, body, contentType, "x")
}

// buildMultipartNamed is buildMultipart with an explicit filename so
// extension-routing tests can drive the dispatch by filename instead
// of MIME.
func buildMultipartNamed(t *testing.T, body, contentType, filename string) (io.Reader, string) {
	t.Helper()
	buf := &bytes.Buffer{}
	mw := multipart.NewWriter(buf)
	h := make(map[string][]string)
	h["Content-Disposition"] = []string{`form-data; name="files"; filename="` + filename + `"`}
	h["Content-Type"] = []string{contentType}
	w, err := mw.CreatePart(h)
	require.NoError(t, err)
	_, err = w.Write([]byte(body))
	require.NoError(t, err)
	require.NoError(t, mw.Close())
	return buf, mw.FormDataContentType()
}

// TestConvert_RoutesByExtensionWhenMimeMisses exercises the default
// strategy (mime_then_extension). When the MIME phase matches no
// engine but the filename's extension does, dispatch must pick the
// extension-matching engine instead of falling all the way through
// to default. Mirrors the .msg bug fix from the integration suite.
func TestConvert_RoutesByExtensionWhenMimeMisses(t *testing.T) {
	t.Parallel()
	defaultHits, msgHits := &atomic.Int32{}, &atomic.Int32{}
	d := &recordingEngine{name: "default-eng", hits: defaultHits}
	msg := &recordingEngine{name: "msg-eng", hits: msgHits}

	registry, err := engine.NewRegistry(map[engine.Name]engine.RegistryEntry{
		"default-eng": {Engine: d},
		"msg-eng":     {Engine: msg, Extensions: []string{".msg"}},
	}, "default-eng", engine.StrategyMimeThenExt)
	require.NoError(t, err)

	h := &Convert{Registry: registry}
	srv := httptest.NewServer(http.HandlerFunc(h.File))
	defer srv.Close()

	body, ct := buildMultipartNamed(t, "x", "text/plain", "report.msg")
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, body)
	require.NoError(t, err)
	req.Header.Set("Content-Type", ct)
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.EqualValues(t, 1, msgHits.Load(), "extension match must beat default fallback")
	require.EqualValues(t, 0, defaultHits.Load())
}

// TestConvert_ExtensionDoesNotOverrideMimeMatch — if both phases
// would claim the request, the registry must respect the strategy's
// declared order. For mime_then_extension that means MIME wins; the
// filename's .msg is irrelevant when the engine ALREADY has a MIME
// claim on the declared Content-Type.
func TestConvert_ExtensionDoesNotOverrideMimeMatch(t *testing.T) {
	t.Parallel()
	pdfHits, msgHits := &atomic.Int32{}, &atomic.Int32{}
	d := &recordingEngine{name: "default-eng", hits: &atomic.Int32{}}
	pdf := &recordingEngine{name: "pdf-eng", hits: pdfHits}
	msg := &recordingEngine{name: "msg-eng", hits: msgHits}

	registry, err := engine.NewRegistry(map[engine.Name]engine.RegistryEntry{
		"default-eng": {Engine: d},
		"pdf-eng":     {Engine: pdf, MimeTypes: []string{"application/pdf"}},
		"msg-eng":     {Engine: msg, Extensions: []string{".msg"}},
	}, "default-eng", engine.StrategyMimeThenExt)
	require.NoError(t, err)

	h := &Convert{Registry: registry}
	srv := httptest.NewServer(http.HandlerFunc(h.File))
	defer srv.Close()

	body, ct := buildMultipartNamed(t, "x", "application/pdf", "x.msg")
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, body)
	require.NoError(t, err)
	req.Header.Set("Content-Type", ct)
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()

	require.EqualValues(t, 1, pdfHits.Load(), "mime_then_extension: MIME phase wins when both match")
	require.EqualValues(t, 0, msgHits.Load())
}

// TestConvert_AccessLogNeverEmptyEngineOnError exercises the
// baseline-stamp fix for the user-reported symptom "engine + engine_url
// come back empty in routing". When buildRequest fails (e.g.,
// malformed Content-Type), the handler returns a 400 without ever
// reaching the dispatch path. Pre-fix, the AccessLog line carried
// empty `engine` + `engine_url`; post-fix the default engine is
// stamped at handler entry so the access log can always attribute
// the request.
func TestConvert_AccessLogNeverEmptyEngineOnError(t *testing.T) {
	t.Parallel()

	defaultHits := &atomic.Int32{}
	d := &recordingEngine{name: "default-eng", hits: defaultHits}
	registry, err := engine.NewRegistry(map[engine.Name]engine.RegistryEntry{
		"default-eng": {Engine: d},
	}, "default-eng", engine.StrategyMimeThenExt)
	require.NoError(t, err)

	// Capture AccessLog output via a buffered zerolog instance.
	logBuf := &bytes.Buffer{}
	logger := zerolog.New(logBuf).With().Timestamp().Logger()
	h := &Convert{Registry: registry, Logger: logger}

	// Wrap the handler in the AccessLog middleware exactly as routes.go
	// does, so the test exercises the real end-to-end stamp pipeline.
	handler := mw.AccessLog(logger)(http.HandlerFunc(h.File))
	srv := httptest.NewServer(handler)
	defer srv.Close()

	// Force buildRequest to fail by sending a non-multipart Content-Type.
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL,
		strings.NewReader(`{"not":"multipart"}`))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode,
		"non-multipart request must 400 BEFORE engine dispatch")

	// Scan the captured log buffer for the request_completed event and
	// assert the engine fields are non-empty even though dispatch never
	// fired.
	var completed map[string]any
	for _, line := range strings.Split(logBuf.String(), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &m))
		if ev, _ := m["event"].(string); ev == "request_completed" {
			completed = m
		}
	}
	require.NotNil(t, completed, "AccessLog must emit request_completed even on 400")
	require.Equal(t, "default-eng", completed["engine"],
		"engine field MUST be stamped at handler entry, not just at dispatch")
	require.NotEmpty(t, completed["engine_url"],
		"engine_url MUST be non-empty on every access-log line for a routed request")
	require.Equal(t, "default", completed["pick_source"],
		"failed dispatch MUST surface pick_source=default")
	require.Equal(t, "mime_then_extension", completed["routing_strategy"])
}

// TestConvert_StrategyExtensionThenMime flips the priority order:
// the extension match wins even when the MIME phase would have
// claimed the request too. Validates that strategy selection
// genuinely controls the order, not just the fall-through.
func TestConvert_StrategyExtensionThenMime(t *testing.T) {
	t.Parallel()
	pdfHits, msgHits := &atomic.Int32{}, &atomic.Int32{}
	d := &recordingEngine{name: "default-eng", hits: &atomic.Int32{}}
	pdf := &recordingEngine{name: "pdf-eng", hits: pdfHits}
	msg := &recordingEngine{name: "msg-eng", hits: msgHits}

	registry, err := engine.NewRegistry(map[engine.Name]engine.RegistryEntry{
		"default-eng": {Engine: d},
		"pdf-eng":     {Engine: pdf, MimeTypes: []string{"application/pdf"}},
		"msg-eng":     {Engine: msg, Extensions: []string{".msg"}},
	}, "default-eng", engine.StrategyExtThenMime)
	require.NoError(t, err)

	h := &Convert{Registry: registry}
	srv := httptest.NewServer(http.HandlerFunc(h.File))
	defer srv.Close()

	body, ct := buildMultipartNamed(t, "x", "application/pdf", "x.msg")
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, body)
	require.NoError(t, err)
	req.Header.Set("Content-Type", ct)
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()

	require.EqualValues(t, 1, msgHits.Load(), "extension_then_mime: extension wins")
	require.EqualValues(t, 0, pdfHits.Load())
}

// TestConvert_FallbackOn5xxRetriesAgainstDefault pins C-31: when
// the primary engine returns a 5xx AND Convert.Fallback is enabled
// AND primary != default, the handler rewinds the file body and
// retries against the default engine. The fallback engine's
// successful response IS the response the caller sees.
func TestConvert_FallbackOn5xxRetriesAgainstDefault(t *testing.T) {
	t.Parallel()
	defaultHits, primaryHits := &atomic.Int32{}, &atomic.Int32{}
	d := &recordingEngine{name: "default-eng", hits: defaultHits}
	primary := &fallback5xxEngine{name: "primary-eng", hits: primaryHits}

	registry, err := engine.NewRegistry(map[engine.Name]engine.RegistryEntry{
		"default-eng": {Engine: d},
		"primary-eng": {Engine: primary, MimeTypes: []string{"application/pdf"}},
	}, "default-eng", engine.StrategyMimeThenExt)
	require.NoError(t, err)

	h := &Convert{Registry: registry, Fallback: true}
	srv := httptest.NewServer(http.HandlerFunc(h.File))
	defer srv.Close()

	body, ct := buildMultipart(t, "x", "application/pdf")
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, body)
	require.NoError(t, err)
	req.Header.Set("Content-Type", ct)
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode,
		"fallback to default MUST recover the 5xx into a 200")
	require.EqualValues(t, 1, primaryHits.Load(), "primary should be hit ONCE")
	require.EqualValues(t, 1, defaultHits.Load(), "default should be hit ONCE on fallback")
}

// TestConvert_FallbackDisabledLeaves5xxAlone — when Fallback is off,
// a primary 5xx propagates unchanged.
func TestConvert_FallbackDisabledLeaves5xxAlone(t *testing.T) {
	t.Parallel()
	d := &recordingEngine{name: "default-eng", hits: &atomic.Int32{}}
	primary := &fallback5xxEngine{name: "primary-eng", hits: &atomic.Int32{}}

	registry, err := engine.NewRegistry(map[engine.Name]engine.RegistryEntry{
		"default-eng": {Engine: d},
		"primary-eng": {Engine: primary, MimeTypes: []string{"application/pdf"}},
	}, "default-eng", engine.StrategyMimeThenExt)
	require.NoError(t, err)

	h := &Convert{Registry: registry, Fallback: false}
	srv := httptest.NewServer(http.HandlerFunc(h.File))
	defer srv.Close()

	body, ct := buildMultipart(t, "x", "application/pdf")
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, body)
	require.NoError(t, err)
	req.Header.Set("Content-Type", ct)
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()

	require.Equal(t, http.StatusInternalServerError, resp.StatusCode,
		"fallback disabled → primary 5xx propagates")
}

// fallback5xxEngine is a stub that always returns 500. Combined with
// recordingEngine for the default, the fallback test asserts the
// retry path.
type fallback5xxEngine struct {
	name engine.Name
	hits *atomic.Int32
}

func (e *fallback5xxEngine) Name() engine.Name { return e.name }
func (e *fallback5xxEngine) URL() string       { return "http://" + string(e.name) + ".test" }
func (e *fallback5xxEngine) Capabilities() engine.EngineCapabilities {
	return engine.EngineCapabilities{Facades: []engine.Facade{engine.FacadeDocling}, HTTPSources: true}
}

func (e *fallback5xxEngine) Convert(_ context.Context, _ *engine.ConvertRequest) (*engine.ConvertResponse, error) {
	e.hits.Add(1)
	return &engine.ConvertResponse{
		StatusCode: http.StatusInternalServerError,
		Body:       io.NopCloser(strings.NewReader(`{"error":"primary down"}`)),
	}, nil
}

func (e *fallback5xxEngine) Health(_ context.Context) error { return nil }
