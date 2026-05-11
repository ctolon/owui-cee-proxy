package docling_test

import (
	"context"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"runtime"
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

// TestDocling_Convert_NoAPIKeyDoesNotStampAuthHeader pins the
// contract: when api_key_env is unset (or resolves to empty), the
// proxy MUST NOT stamp the configured auth header. A header with an
// empty value would still be sent and some backends treat the
// presence of "X-Api-Key:" as an attempted auth, returning 401.
func TestDocling_Convert_NoAPIKeyDoesNotStampAuthHeader(t *testing.T) {
	t.Parallel()

	var sawAuthHeader bool
	var rawHeaderValue string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, sawAuthHeader = r.Header["X-Api-Key"]
		rawHeaderValue = r.Header.Get("X-Api-Key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	a := newDoclingAdapter(t, srv.URL, func(c *config.EngineConfig) {
		c.APIKey = "" // simulate unresolved api_key_env
	})

	resp, err := a.Convert(context.Background(), &engine.ConvertRequest{
		Facade: engine.FacadeDocling,
		Files: []engine.FileBlob{{
			Filename: "x.txt", ContentType: "text/plain", Body: strings.NewReader("hi"),
		}},
	})
	require.NoError(t, err)
	defer resp.Body.Close()
	require.False(t, sawAuthHeader, "X-Api-Key must not be set when APIKey is empty (got %q)", rawHeaderValue)
}

// TestDocling_Convert_EmptyAuthHeaderDoesNotStampEvenWithKey pins
// the inverse: a non-empty APIKey paired with an empty AuthHeader
// must not stamp anything. There's no canonical header to use, so
// silently doing nothing is the only safe choice.
func TestDocling_Convert_EmptyAuthHeaderDoesNotStampEvenWithKey(t *testing.T) {
	t.Parallel()

	var headerCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Count any header that looks key-ish to catch a regression
		// that picks an arbitrary fallback header.
		for k := range r.Header {
			lower := strings.ToLower(k)
			if strings.Contains(lower, "api") || strings.Contains(lower, "auth") || strings.Contains(lower, "key") {
				headerCount++
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	a := newDoclingAdapter(t, srv.URL, func(c *config.EngineConfig) {
		c.AuthHeader = ""
	})

	resp, err := a.Convert(context.Background(), &engine.ConvertRequest{
		Facade: engine.FacadeDocling,
		Files: []engine.FileBlob{{
			Filename: "x.txt", ContentType: "text/plain", Body: strings.NewReader("hi"),
		}},
	})
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Zero(t, headerCount, "no auth-like header should appear when AuthHeader is empty")
}

// TestDocling_Convert_SourceModeAppliesAuth ensures the convertSource
// (JSON, /v1/convert/source) path applies auth identically to the
// file path. Without this test a regression that only wires ApplyAuth
// into convertFile would slip past CI.
func TestDocling_Convert_SourceModeAppliesAuth(t *testing.T) {
	t.Parallel()

	var seenAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("X-Api-Key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	a := newDoclingAdapter(t, srv.URL, nil)

	resp, err := a.Convert(context.Background(), &engine.ConvertRequest{
		Facade:  engine.FacadeDocling,
		Sources: []engine.HTTPSource{{URL: "https://example.com/x.pdf"}},
	})
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, "the-key", seenAuth, "source-mode requests must also receive the configured API key")
}

// TestApplyAuth_TableDriven covers the standalone helper that is also
// reused by sibling adapters (doclingexternal composite).
func TestApplyAuth_TableDriven(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		key        string
		header     string
		scheme     string
		wantHeader string // empty means: header must not be present
		wantValue  string
	}{
		{name: "raw with key", key: "secret123", header: "X-Api-Key", scheme: "raw", wantHeader: "X-Api-Key", wantValue: "secret123"},
		{name: "bearer with key", key: "secret123", header: "Authorization", scheme: "bearer", wantHeader: "Authorization", wantValue: "Bearer secret123"},
		{name: "empty key omits header", key: "", header: "X-Api-Key", scheme: "raw"},
		{name: "empty header omits stamping", key: "secret123", header: "", scheme: "raw"},
		{name: "unknown scheme falls back to raw", key: "secret123", header: "X-Api-Key", scheme: "weird", wantHeader: "X-Api-Key", wantValue: "secret123"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := http.Header{}
			docling.ApplyAuth(h, config.EngineConfig{
				APIKey:     config.Secret(tc.key),
				AuthHeader: tc.header,
				AuthScheme: config.AuthScheme(tc.scheme),
			})
			if tc.wantHeader == "" {
				require.Empty(t, h, "no headers expected, got %+v", h)
				return
			}
			require.Equal(t, tc.wantValue, h.Get(tc.wantHeader))
		})
	}
}

// TestDocling_ConvertFile_NoLeakOnEarlyReturn pins C-12 from the
// FAANG review: when http.NewRequestWithContext fails AFTER the
// producer goroutine has been spawned (rare cold-path, e.g.,
// malformed URL with control bytes), the goroutine MUST exit
// instead of blocking forever on io.Copy into a never-read pipe.
//
// The trigger here is a URL containing a NUL byte — net/url rejects
// it inside http.NewRequestWithContext. Pre-fix, the producer
// goroutine would sit on io.Copy(fw, f.Body) until process exit;
// post-fix, pr.CloseWithError unwinds it before the function
// returns.
//
// Detection: snapshot runtime.NumGoroutine() before + after a small
// settle window. A leaked goroutine survives past the settle and
// fails the assertion.
func TestDocling_ConvertFile_NoLeakOnEarlyReturn(t *testing.T) {
	// Don't t.Parallel — the goroutine-count assertion is sensitive
	// to siblings.
	a := newDoclingAdapter(t, "http://valid-base.invalid/\x00ctrl", nil)
	req := &engine.ConvertRequest{
		Files: []engine.FileBlob{{
			Filename:    "x.pdf",
			ContentType: "application/pdf",
			Body:        strings.NewReader("dummy body that the producer would block writing"),
		}},
	}

	// Let any background fixtures from earlier subtests settle.
	time.Sleep(50 * time.Millisecond)
	before := runtime.NumGoroutine()

	_, err := a.Convert(context.Background(), req)
	require.Error(t, err, "control-byte URL MUST fail http.NewRequestWithContext")

	// Give pr.CloseWithError time to propagate to the producer
	// goroutine's next pw.Write call.
	time.Sleep(200 * time.Millisecond)
	after := runtime.NumGoroutine()
	require.LessOrEqual(t, after, before+1,
		"convertFile early-return path MUST NOT leak the producer goroutine (before=%d after=%d)",
		before, after)
}
