package tika_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ctolon/owui-cee-proxy/internal/config"
	"github.com/ctolon/owui-cee-proxy/internal/engine"
	"github.com/ctolon/owui-cee-proxy/internal/engine/compat/tika"
	"github.com/ctolon/owui-cee-proxy/internal/engine/enginetest"
	"github.com/ctolon/owui-cee-proxy/internal/httpclient"
)

func newTikaAdapter(t *testing.T, base string) *tika.Adapter {
	t.Helper()
	cfg := config.EngineConfig{
		Enable:         true,
		CompatType:     config.CompatTika,
		URL:            base,
		AuthHeader:     "X-Api-Key",
		AuthScheme:     "raw",
		RequestTimeout: 5 * time.Second,
	}
	c, err := httpclient.New(cfg)
	require.NoError(t, err)
	a, err := tika.New("legacy-tika", cfg, c, nil)
	require.NoError(t, err)
	return a
}

// fakeTika returns the canonical Tika single-object response shape.
func fakeTika(t *testing.T) (*httptest.Server, *string) {
	t.Helper()
	var seenPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		require.Equal(t, http.MethodPut, r.Method)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"X-TIKA:content":"extracted text","Content-Length":"42"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &seenPath
}

func TestTika_Convert_DoclingFacadeWrapsAsDoclingEnvelope(t *testing.T) {
	t.Parallel()
	srv, path := fakeTika(t)
	a := newTikaAdapter(t, srv.URL)

	resp, err := a.Convert(context.Background(), &engine.ConvertRequest{
		Facade: engine.FacadeDocling,
		Files: []engine.FileBlob{{
			Filename: "x.pdf", ContentType: "application/pdf", Body: strings.NewReader("FAKEPDF"),
		}},
	})
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "/tika/text", *path)

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	var parsed map[string]any
	require.NoError(t, json.Unmarshal(raw, &parsed))
	require.Equal(t, "success", parsed["status"])
	doc, ok := parsed["document"].(map[string]any)
	require.True(t, ok, "docling envelope must have a document key")
	require.Equal(t, "extracted text", doc["md_content"])
}

func TestTika_Convert_ExternalFacadeWrapsAsPageContent(t *testing.T) {
	t.Parallel()
	srv, _ := fakeTika(t)
	a := newTikaAdapter(t, srv.URL)

	resp, err := a.Convert(context.Background(), &engine.ConvertRequest{
		Facade: engine.FacadeExternal,
		Files: []engine.FileBlob{{
			Filename: "x.pdf", ContentType: "application/pdf", Body: strings.NewReader("FAKEPDF"),
		}},
	})
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	// external single-file shape is a top-level object.
	var parsed map[string]any
	require.NoError(t, json.Unmarshal(raw, &parsed))
	require.Equal(t, "extracted text", parsed["page_content"])
	md, ok := parsed["metadata"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "x.pdf", md["filename"])
}

func TestTika_Convert_RejectsHTTPSourcesPerFacade(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("backend must not be hit when only Sources are present")
	}))
	t.Cleanup(srv.Close)
	a := newTikaAdapter(t, srv.URL)

	cases := []engine.Facade{engine.FacadeDocling, engine.FacadeExternal}
	for _, f := range cases {
		t.Run(string(f), func(t *testing.T) {
			t.Parallel()
			resp, err := a.Convert(context.Background(), &engine.ConvertRequest{
				Facade:  f,
				Sources: []engine.HTTPSource{{URL: "http://example.com/x.pdf"}},
			})
			require.NoError(t, err)
			defer resp.Body.Close()
			require.Equal(t, http.StatusNotImplemented, resp.StatusCode)
		})
	}
}

func TestTika_Capabilities(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	a := newTikaAdapter(t, srv.URL)
	caps := a.Capabilities()
	require.True(t, caps.AcceptsFacade(engine.FacadeDocling))
	require.True(t, caps.AcceptsFacade(engine.FacadeExternal))
	require.False(t, caps.HTTPSources)
}

func TestTika_ContractTests(t *testing.T) {
	t.Parallel()
	srv, _ := fakeTika(t)
	a := newTikaAdapter(t, srv.URL)
	enginetest.RunContractTests(t, a)
}
