package kreuzberg

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

func TestConvert_ForwardsToDoclingCompatEndpoint(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "POST", r.Method)
		require.Equal(t, "/v1/convert/file", r.URL.Path,
			"adapter must hit Kreuzberg's docling-compat endpoint")
		require.Equal(t, "Bearer secret", r.Header.Get("Authorization"))

		mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		require.NoError(t, err)
		require.Equal(t, "multipart/form-data", mt)

		mr := multipart.NewReader(r.Body, params["boundary"])
		fields := map[string]string{}
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			require.NoError(t, err)
			b, _ := io.ReadAll(part)
			if part.FileName() != "" {
				fields["file:"+part.FileName()] = string(b)
			} else {
				fields[part.FormName()] = string(b)
			}
		}
		require.Equal(t, "abc", fields["file:doc.pdf"])
		require.Equal(t, "markdown", fields["output_format"],
			"forward_options should be passed through as form fields")

		// Kreuzberg already returns a Docling-shaped envelope.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"document":{"md_content":"# Hello"},"status":"success"}`))
	}))
	defer srv.Close()

	cfg := config.EngineConfig{
		URL:    srv.URL,
		APIKey: "secret",
		ForwardOptions: map[string]string{
			"output_format": "markdown",
		},
	}
	cli, _ := httpclient.New(cfg)
	a, err := New(cfg, cli, nil)
	require.NoError(t, err)

	resp, err := a.Convert(context.Background(), &engine.ConvertRequest{
		Files: []engine.FileBlob{
			{Filename: "doc.pdf", ContentType: "application/pdf", Body: strings.NewReader("abc")},
		},
		RequestID: "req-1",
	})
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	body, _ := io.ReadAll(resp.Body)
	require.Contains(t, string(body), "md_content")
	require.Contains(t, string(body), "Hello")
}

func TestConvert_RejectsHTTPSourcesWith501(t *testing.T) {
	t.Parallel()

	cfg := config.EngineConfig{URL: "http://example.invalid"}
	cli, _ := httpclient.New(cfg)
	a, _ := New(cfg, cli, nil)

	resp, err := a.Convert(context.Background(), &engine.ConvertRequest{
		Sources: []engine.HTTPSource{{URL: "http://example.invalid/x"}},
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusNotImplemented, resp.StatusCode)
	defer resp.Body.Close()
}
