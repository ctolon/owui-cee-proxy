package docling

import (
	"context"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ctolon/owui-cee-proxy/internal/config"
	"github.com/ctolon/owui-cee-proxy/internal/engine"
	"github.com/ctolon/owui-cee-proxy/internal/httpclient"
)

func TestConvertFile_StreamsMultipartAndForwardsAPIKey(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "POST", r.Method)
		require.Equal(t, "/v1/convert/file", r.URL.Path)
		require.Equal(t, "secret-key", r.Header.Get("X-Api-Key"))

		mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		require.NoError(t, err)
		require.Equal(t, "multipart/form-data", mediaType)

		mr := multipart.NewReader(r.Body, params["boundary"])
		got := map[string]string{}
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			require.NoError(t, err)
			b, err := io.ReadAll(part)
			require.NoError(t, err)
			if part.FileName() != "" {
				got["__file__"+part.FormName()] = part.FileName() + ":" + string(b)
			} else {
				got[part.FormName()] = string(b)
			}
			_ = part.Close()
		}

		require.Equal(t, "sample.pdf:hello-bytes", got["__file__files"])
		require.Equal(t, "placeholder", got["image_export_mode"])

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"document":{"md_content":"# hi"}}`))
	}))
	defer srv.Close()

	cfg := config.EngineConfig{URL: srv.URL, APIKey: "secret-key"}
	cli, err := httpclient.New(cfg)
	require.NoError(t, err)

	a, err := New(cfg, cli, nil)
	require.NoError(t, err)

	req := &engine.ConvertRequest{
		Files: []engine.FileBlob{
			{Filename: "sample.pdf", ContentType: "application/pdf", Body: strings.NewReader("hello-bytes")},
		},
		Options: map[string]string{"image_export_mode": "placeholder"},
	}
	resp, err := a.Convert(context.Background(), req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	require.Contains(t, string(body), "md_content")
}

func TestConvertSource_SendsJSON(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/convert/source", r.URL.Path)
		require.Equal(t, "application/json", r.Header.Get("Content-Type"))
		body, _ := io.ReadAll(r.Body)
		require.Contains(t, string(body), `"http_sources"`)
		require.Contains(t, string(body), "https://example.com/x.pdf")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	cfg := config.EngineConfig{URL: srv.URL}
	cli, err := httpclient.New(cfg)
	require.NoError(t, err)

	a, err := New(cfg, cli, nil)
	require.NoError(t, err)

	resp, err := a.Convert(context.Background(), &engine.ConvertRequest{
		Sources: []engine.HTTPSource{{URL: "https://example.com/x.pdf"}},
	})
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHealth(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/health", r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := config.EngineConfig{URL: srv.URL}
	cli, _ := httpclient.New(cfg)
	a, _ := New(cfg, cli, nil)
	require.NoError(t, a.Health(context.Background()))
}
