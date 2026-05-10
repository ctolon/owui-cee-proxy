package docling_test

import (
	"context"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ctolon/owui-cee-proxy/internal/config"
	"github.com/ctolon/owui-cee-proxy/internal/engine"
	"github.com/ctolon/owui-cee-proxy/internal/engine/compat/docling"
	"github.com/ctolon/owui-cee-proxy/internal/engine/enginetest"
	"github.com/ctolon/owui-cee-proxy/internal/httpclient"
)

func newDoclingAdapter(t *testing.T, base string, mut func(*config.EngineConfig)) *docling.Adapter {
	t.Helper()
	cfg := config.EngineConfig{
		Enable:         true,
		CompatType:     config.CompatDocling,
		URL:            base,
		APIKey:         "the-key",
		AuthHeader:     "X-Api-Key",
		AuthScheme:     "raw",
		RequestTimeout: 5 * time.Second,
	}
	if mut != nil {
		mut(&cfg)
	}
	c, err := httpclient.New(cfg)
	require.NoError(t, err)
	a, err := docling.New("main-docling", cfg, c, nil)
	require.NoError(t, err)
	return a
}

func TestDocling_Convert_FileMultipartHonorsAuthRaw(t *testing.T) {
	t.Parallel()

	var (
		seenPath  string
		seenAuth  string
		seenAuth2 string
		gotPart   string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenAuth = r.Header.Get("X-Api-Key")
		seenAuth2 = r.Header.Get("Authorization")

		mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		require.NoError(t, err)
		require.Equal(t, "multipart/form-data", mt)
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			require.NoError(t, err)
			if p.FormName() == "files" {
				b, _ := io.ReadAll(p)
				gotPart = string(b)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"document":{"md_content":"hi"}}`))
	}))
	defer srv.Close()

	a := newDoclingAdapter(t, srv.URL, nil)

	resp, err := a.Convert(context.Background(), &engine.ConvertRequest{
		Facade: engine.FacadeDocling,
		Files: []engine.FileBlob{{
			Filename: "x.txt", ContentType: "text/plain", Body: strings.NewReader("hello"),
		}},
	})
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "/v1/convert/file", seenPath)
	require.Equal(t, "the-key", seenAuth)
	require.Empty(t, seenAuth2, "raw auth scheme must not also set Authorization")
	require.Equal(t, "hello", gotPart)
}

func TestDocling_Convert_AuthSchemeBearerPrependsBearerPrefix(t *testing.T) {
	t.Parallel()
	var seenAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"document":{"md_content":"ok"}}`))
	}))
	defer srv.Close()

	a := newDoclingAdapter(t, srv.URL, func(c *config.EngineConfig) {
		c.AuthHeader = "Authorization"
		c.AuthScheme = "bearer"
	})

	resp, err := a.Convert(context.Background(), &engine.ConvertRequest{
		Facade: engine.FacadeDocling,
		Files: []engine.FileBlob{{
			Filename: "x.txt", ContentType: "text/plain", Body: strings.NewReader("hi"),
		}},
	})
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, "Bearer the-key", seenAuth)
}

func TestDocling_Convert_PathOverride(t *testing.T) {
	t.Parallel()
	var seenPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	a := newDoclingAdapter(t, srv.URL, func(c *config.EngineConfig) {
		c.Paths = config.EnginePathsConfig{DoclingConvertFile: "/api/v2/extract"}
	})

	resp, err := a.Convert(context.Background(), &engine.ConvertRequest{
		Facade: engine.FacadeDocling,
		Files: []engine.FileBlob{{
			Filename: "x.txt", ContentType: "text/plain", Body: strings.NewReader("hi"),
		}},
	})
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, "/api/v2/extract", seenPath)
}

func TestDocling_Convert_SourceModeHitsConvertSource(t *testing.T) {
	t.Parallel()
	var seenPath, seenCT, seenBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenCT = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		seenBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"document":{"md_content":"src"}}`))
	}))
	defer srv.Close()

	a := newDoclingAdapter(t, srv.URL, nil)

	resp, err := a.Convert(context.Background(), &engine.ConvertRequest{
		Facade:  engine.FacadeDocling,
		Sources: []engine.HTTPSource{{URL: "https://example.com/x.pdf"}},
	})
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, "/v1/convert/source", seenPath)
	require.Equal(t, "application/json", seenCT)
	require.Contains(t, seenBody, "https://example.com/x.pdf")
}

func TestDocling_Convert_NormalizesNullMdContent(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"document":{"md_content":null,"text_content":null}}`))
	}))
	defer srv.Close()

	a := newDoclingAdapter(t, srv.URL, nil)
	resp, err := a.Convert(context.Background(), &engine.ConvertRequest{
		Facade: engine.FacadeDocling,
		Files: []engine.FileBlob{{
			Filename: "x.txt", ContentType: "text/plain", Body: strings.NewReader("x"),
		}},
	})
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	require.Contains(t, string(body), `"md_content":""`)
	require.Contains(t, string(body), `"text_content":""`)
	require.NotContains(t, string(body), `"md_content":null`)
}

func TestDocling_Capabilities(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	a := newDoclingAdapter(t, srv.URL, nil)
	caps := a.Capabilities()
	require.Equal(t, []engine.Facade{engine.FacadeDocling}, caps.Facades)
	require.True(t, caps.HTTPSources)
}

func TestDocling_ContractTests(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"document":{"md_content":""}}`))
	}))
	defer srv.Close()
	a := newDoclingAdapter(t, srv.URL, nil)
	enginetest.RunContractTests(t, a)
}

func TestDocling_SafeMultipartFilename_RejectsCRLF(t *testing.T) {
	t.Parallel()
	require.Equal(t, "", docling.SafeMultipartFilename("x\r\ny.txt"))
	require.Equal(t, "", docling.SafeMultipartFilename("x\x00y.txt"))
	require.Equal(t, `x\"y.txt`, docling.SafeMultipartFilename(`x"y.txt`))
}
