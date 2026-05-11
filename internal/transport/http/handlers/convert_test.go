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

	"github.com/stretchr/testify/require"

	"github.com/ctolon/owui-cee-proxy/internal/engine"
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
	}, "default-eng")
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
	}, "default-eng")
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
	}, "primary")
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
	}, "primary")
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
	}, "primary")
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

func buildMultipart(t *testing.T, body, contentType string) (io.Reader, string) {
	t.Helper()
	buf := &bytes.Buffer{}
	mw := multipart.NewWriter(buf)
	h := make(map[string][]string)
	h["Content-Disposition"] = []string{`form-data; name="files"; filename="x"`}
	h["Content-Type"] = []string{contentType}
	w, err := mw.CreatePart(h)
	require.NoError(t, err)
	_, err = w.Write([]byte(body))
	require.NoError(t, err)
	require.NoError(t, mw.Close())
	return buf, mw.FormDataContentType()
}
