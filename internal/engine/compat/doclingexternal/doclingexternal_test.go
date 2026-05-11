package doclingexternal_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ctolon/owui-cee-proxy/internal/config"
	"github.com/ctolon/owui-cee-proxy/internal/engine"
	"github.com/ctolon/owui-cee-proxy/internal/engine/compat/doclingexternal"
	"github.com/ctolon/owui-cee-proxy/internal/httpclient"
)

func newComposite(t *testing.T, base string) *doclingexternal.Adapter {
	t.Helper()
	cfg := config.EngineConfig{
		Enable:         true,
		CompatType:     config.CompatDoclingExternal,
		URL:            base,
		APIKey:         "key",
		AuthHeader:     "X-Api-Key",
		AuthScheme:     "raw",
		RequestTimeout: 5 * time.Second,
	}
	c, err := httpclient.New(cfg)
	require.NoError(t, err)
	a, err := doclingexternal.New("hybrid", cfg, c, nil)
	require.NoError(t, err)
	return a
}

func TestDoclingExternal_Capabilities(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	a := newComposite(t, srv.URL)

	caps := a.Capabilities()
	require.True(t, caps.AcceptsFacade(engine.FacadeDocling))
	require.True(t, caps.AcceptsFacade(engine.FacadeExternal))
	require.True(t, caps.HTTPSources)
}

func TestDoclingExternal_DispatchesByFacade(t *testing.T) {
	t.Parallel()

	var doclingHits, externalHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/convert/file":
			doclingHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"document":{"md_content":"DOC"}}`))
		case "/process":
			externalHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"page_content":"EXT"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	a := newComposite(t, srv.URL)

	// docling facade → multipart endpoint
	rDoc := &engine.ConvertRequest{
		Facade: engine.FacadeDocling,
		Files: []engine.FileBlob{{
			Filename: "x.txt", ContentType: "text/plain", Body: strings.NewReader("hi"),
		}},
	}
	resp, err := a.Convert(context.Background(), rDoc)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// external facade → /process raw PUT
	rExt := &engine.ConvertRequest{
		Facade: engine.FacadeExternal,
		Files: []engine.FileBlob{{
			Filename: "y.txt", ContentType: "text/plain", Body: strings.NewReader("yo"),
		}},
	}
	resp2, err := a.Convert(context.Background(), rExt)
	require.NoError(t, err)
	defer resp2.Body.Close()
	require.Equal(t, http.StatusOK, resp2.StatusCode)

	require.EqualValues(t, 1, doclingHits.Load(), "docling child got the docling-facade request")
	require.EqualValues(t, 1, externalHits.Load(), "external child got the external-facade request")
}

// TestDoclingExternal_AuthAppliedOnBothFacades verifies that a single
// API key configured on the composite is forwarded to BOTH child
// adapters: the docling-shaped /v1/convert/file path AND the
// external-shaped /process path. A regression that wires auth into
// one child but not the other would be invisible until production.
func TestDoclingExternal_AuthAppliedOnBothFacades(t *testing.T) {
	t.Parallel()

	var doclingAuth, externalAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/convert/file":
			doclingAuth = r.Header.Get("X-Api-Key")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"document":{"md_content":"DOC"}}`))
		case "/process":
			externalAuth = r.Header.Get("X-Api-Key")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"page_content":"EXT"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	a := newComposite(t, srv.URL)

	respDoc, err := a.Convert(context.Background(), &engine.ConvertRequest{
		Facade: engine.FacadeDocling,
		Files: []engine.FileBlob{{
			Filename: "x.txt", ContentType: "text/plain", Body: strings.NewReader("hi"),
		}},
	})
	require.NoError(t, err)
	respDoc.Body.Close()

	respExt, err := a.Convert(context.Background(), &engine.ConvertRequest{
		Facade: engine.FacadeExternal,
		Files: []engine.FileBlob{{
			Filename: "y.txt", ContentType: "text/plain", Body: strings.NewReader("yo"),
		}},
	})
	require.NoError(t, err)
	respExt.Body.Close()

	require.Equal(t, "key", doclingAuth, "docling-facade request must carry the API key")
	require.Equal(t, "key", externalAuth, "external-facade request must carry the API key")
}

func TestDoclingExternal_RejectsUnknownFacade(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	a := newComposite(t, srv.URL)
	_, err := a.Convert(context.Background(), &engine.ConvertRequest{Facade: engine.Facade("bogus")})
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown facade")
}
