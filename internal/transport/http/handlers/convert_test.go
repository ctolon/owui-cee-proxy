package handlers

import (
	"bytes"
	"context"
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

	doclingHits, tikaHits, kbHits := &atomic.Int32{}, &atomic.Int32{}, &atomic.Int32{}
	d := &recordingEngine{name: engine.Docling, hits: doclingHits}
	tk := &recordingEngine{name: engine.Tika, hits: tikaHits}
	kb := &recordingEngine{name: engine.Kreuzberg, hits: kbHits}

	registry, err := engine.NewRegistry(map[engine.Name]engine.RegistryEntry{
		engine.Docling:   {Engine: d},
		engine.Tika:      {Engine: tk, MimeTypes: []string{"application/pdf"}},
		engine.Kreuzberg: {Engine: kb, MimeTypes: []string{"image/*"}},
	}, engine.Docling)
	require.NoError(t, err)

	h := &Convert{Registry: registry}
	srv := httptest.NewServer(http.HandlerFunc(h.File))
	defer srv.Close()

	cases := []struct {
		mime    string
		want    *atomic.Int32
		wantOne int32
	}{
		{"application/pdf", tikaHits, 1},
		{"image/png", kbHits, 1},
		{"application/octet-stream", doclingHits, 1},
		{"text/plain", doclingHits, 2}, // catches second "no match" case
	}

	for _, c := range cases {
		body, contentType := buildMultipart(t, "x", c.mime)
		resp, err := http.Post(srv.URL, contentType, body)
		require.NoError(t, err, "mime=%s", c.mime)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.EqualValues(t, c.wantOne, c.want.Load(), "mime=%s wrong engine", c.mime)
	}
}

func TestConvert_SourceModeUsesDefault(t *testing.T) {
	t.Parallel()
	doclingHits, tikaHits := &atomic.Int32{}, &atomic.Int32{}
	d := &recordingEngine{name: engine.Docling, hits: doclingHits}
	tk := &recordingEngine{name: engine.Tika, hits: tikaHits}

	registry, err := engine.NewRegistry(map[engine.Name]engine.RegistryEntry{
		engine.Docling: {Engine: d},
		engine.Tika:    {Engine: tk, MimeTypes: []string{"application/pdf"}},
	}, engine.Docling)
	require.NoError(t, err)

	h := &Convert{Registry: registry}
	srv := httptest.NewServer(http.HandlerFunc(h.Source))
	defer srv.Close()

	resp, err := http.Post(srv.URL, "application/json", strings.NewReader(
		`{"http_sources":[{"url":"https://1.1.1.1/x.pdf"}]}`,
	))
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Zero(t, tikaHits.Load(), "source mode must NOT route by mime")
	require.EqualValues(t, 1, doclingHits.Load())
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
