package external_test

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
	"github.com/ctolon/owui-cee-proxy/internal/engine/compat/external"
	"github.com/ctolon/owui-cee-proxy/internal/engine/enginetest"
	"github.com/ctolon/owui-cee-proxy/internal/httpclient"
)

func newTestAdapter(t *testing.T, base string, paths config.EnginePathsConfig) *external.Adapter {
	t.Helper()
	cfg := config.EngineConfig{
		Enable:         true,
		CompatType:     config.CompatExternal,
		URL:            base,
		APIKey:         "test-token",
		AuthHeader:     "Authorization",
		AuthScheme:     "bearer",
		RequestTimeout: 5 * time.Second,
		Paths:          paths,
	}
	c, err := httpclient.New(cfg)
	require.NoError(t, err)
	a, err := external.New("ext-test", cfg, c, nil)
	require.NoError(t, err)
	return a
}

func TestExternal_Convert_PutRawWithBearerAndFilename(t *testing.T) {
	t.Parallel()

	var (
		seenMethod   string
		seenPath     string
		seenAuth     string
		seenCT       string
		seenFilename string
		seenBody     string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenMethod = r.Method
		seenPath = r.URL.Path
		seenAuth = r.Header.Get("Authorization")
		seenCT = r.Header.Get("Content-Type")
		seenFilename = r.Header.Get("X-Filename")
		body, _ := io.ReadAll(r.Body)
		seenBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"page_content":"hello world","metadata":{"source":"x.txt"}}`))
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL, config.EnginePathsConfig{})

	resp, err := a.Convert(context.Background(), &engine.ConvertRequest{
		Facade: engine.FacadeExternal,
		Files: []engine.FileBlob{{
			Filename:    "x.txt",
			ContentType: "text/plain",
			Body:        strings.NewReader("hello world"),
		}},
	})
	require.NoError(t, err)
	require.NotNil(t, resp)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, http.MethodPut, seenMethod)
	require.Equal(t, "/process", seenPath)
	require.Equal(t, "Bearer test-token", seenAuth)
	require.Equal(t, "text/plain", seenCT)
	require.Equal(t, "x.txt", seenFilename)
	require.Equal(t, "hello world", seenBody)
}

func TestExternal_Convert_BatchedAggregatesIntoList(t *testing.T) {
	t.Parallel()

	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		body, _ := json.Marshal(map[string]any{
			"page_content": "doc " + r.Header.Get("X-Filename"),
			"metadata":     map[string]any{"i": hits},
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL, config.EnginePathsConfig{})

	resp, err := a.Convert(context.Background(), &engine.ConvertRequest{
		Facade: engine.FacadeExternal,
		Files: []engine.FileBlob{
			{Filename: "a.txt", ContentType: "text/plain", Body: strings.NewReader("A")},
			{Filename: "b.txt", ContentType: "text/plain", Body: strings.NewReader("B")},
		},
	})
	require.NoError(t, err)
	require.NotNil(t, resp)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, 2, hits)

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	var list []map[string]any
	require.NoError(t, json.Unmarshal(raw, &list))
	require.Len(t, list, 2)
	require.Equal(t, "doc a.txt", list[0]["page_content"])
	require.Equal(t, "doc b.txt", list[1]["page_content"])
}

func TestExternal_Convert_RejectsHTTPSources(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("backend must not be hit when only Sources are present")
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL, config.EnginePathsConfig{})

	resp, err := a.Convert(context.Background(), &engine.ConvertRequest{
		Facade:  engine.FacadeExternal,
		Sources: []engine.HTTPSource{{URL: "http://example.com/x.pdf"}},
	})
	require.NoError(t, err)
	require.NotNil(t, resp)
	defer resp.Body.Close()
	require.Equal(t, http.StatusNotImplemented, resp.StatusCode)
}

func TestExternal_Convert_HonoursPathOverride(t *testing.T) {
	t.Parallel()
	var seenPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"page_content":"ok"}`))
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL, config.EnginePathsConfig{ExternalProcess: "/custom/process"})

	resp, err := a.Convert(context.Background(), &engine.ConvertRequest{
		Facade: engine.FacadeExternal,
		Files: []engine.FileBlob{{
			Filename: "x.txt", ContentType: "text/plain", Body: strings.NewReader("hi"),
		}},
	})
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, "/custom/process", seenPath)
}

func TestExternal_Capabilities(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	a := newTestAdapter(t, srv.URL, config.EnginePathsConfig{})
	caps := a.Capabilities()
	require.Equal(t, []engine.Facade{engine.FacadeExternal}, caps.Facades)
	require.False(t, caps.HTTPSources, "external loader does not support http sources")
}

func TestExternal_ContractTests(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"page_content":"ok"}`))
	}))
	defer srv.Close()
	a := newTestAdapter(t, srv.URL, config.EnginePathsConfig{})
	enginetest.RunContractTests(t, a)
}
