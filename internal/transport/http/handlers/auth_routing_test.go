package handlers_test

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/ctolon/owui-cee-proxy/internal/breaker"
	"github.com/ctolon/owui-cee-proxy/internal/config"
	"github.com/ctolon/owui-cee-proxy/internal/engine"
	"github.com/ctolon/owui-cee-proxy/internal/engine/compat/docling"
	"github.com/ctolon/owui-cee-proxy/internal/httpclient"
	"github.com/ctolon/owui-cee-proxy/internal/transport/http/handlers"
)

// TestAuth_RouteByMIME_NonDefaultEngine_StampsCorrectKey reproduces
// the user-reported bug: when default_engine is NOT docling, a
// docling-compat engine that owns the MIME (via mime_types) must
// still receive ITS OWN API key on the outbound request — not the
// default engine's key, and not an empty header.
//
// The repro config mirrors the user's deployment:
//
//	default_engine = "fallback"
//	engines:
//	  fallback:  compat=docling, api_key="fallback-key"     # catch-all
//	  primary:   compat=docling, api_key="primary-key",     # owns PDF
//	             mime_types: ["application/pdf"]
//
// A PDF upload MUST route to "primary" AND the primary backend
// MUST see "X-Api-Key: primary-key" (not "fallback-key", not "").
func TestAuth_RouteByMIME_NonDefaultEngine_StampsCorrectKey(t *testing.T) {
	t.Parallel()

	// Two echo backends. Each captures the X-Api-Key it received so
	// the test can assert which engine the proxy actually dispatched
	// to AND what auth value it stamped.
	var primaryHits, fallbackHits atomic.Int32
	var primarySeenKey, fallbackSeenKey atomic.Value
	primarySeenKey.Store("")
	fallbackSeenKey.Store("")

	primaryBE := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryHits.Add(1)
		primarySeenKey.Store(r.Header.Get("X-Api-Key"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"document":{"md_content":"primary ok"}}`))
	}))
	defer primaryBE.Close()

	fallbackBE := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackHits.Add(1)
		fallbackSeenKey.Store(r.Header.Get("X-Api-Key"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"document":{"md_content":"fallback ok"}}`))
	}))
	defer fallbackBE.Close()

	// Adapter wiring — matches what app.go::buildRegistry does (minus
	// the metric callbacks, which aren't load-bearing for THIS test).
	mkAdapter := func(t *testing.T, name, url, key string, mimes []string) (engine.Engine, []string) {
		t.Helper()
		ec := config.EngineConfig{
			Enable:         true,
			CompatType:     config.CompatDocling,
			URL:            url,
			APIKey:         config.Secret(key),
			AuthHeader:     "X-Api-Key",
			AuthScheme:     config.AuthSchemeRaw,
			RequestTimeout: 5 * time.Second,
			MimeTypes:      mimes,
		}
		c, err := httpclient.New(ec)
		require.NoError(t, err)
		br := breaker.New(name, ec.Breaker, nil)
		ad, err := docling.New(engine.Name(name), ec, c, br)
		require.NoError(t, err)
		return ad, mimes
	}

	primaryEng, primaryMimes := mkAdapter(t, "primary", primaryBE.URL, "primary-key", []string{"application/pdf"})
	fallbackEng, _ := mkAdapter(t, "fallback", fallbackBE.URL, "fallback-key", nil)

	registry, err := engine.NewRegistry(map[engine.Name]engine.RegistryEntry{
		"primary":  {Engine: primaryEng, MimeTypes: primaryMimes},
		"fallback": {Engine: fallbackEng},
	}, "fallback", "")
	require.NoError(t, err)

	convert := &handlers.Convert{Registry: registry, Logger: zerolog.Nop()}
	front := httptest.NewServer(http.HandlerFunc(convert.File))
	defer front.Close()

	// Send a PDF — MIME-based dispatch must pick "primary".
	body, ctype := buildPDFMultipart(t, "test content")
	resp, err := http.Post(front.URL, ctype, body)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	require.EqualValues(t, 1, primaryHits.Load(), "primary backend MUST receive the PDF (MIME-match wins over default)")
	require.EqualValues(t, 0, fallbackHits.Load(), "fallback (default) MUST NOT see the PDF")
	require.Equal(t, "primary-key", primarySeenKey.Load().(string),
		"primary backend must receive primary-key (the per-engine API key), NOT the default engine's key")
	require.Empty(t, fallbackSeenKey.Load().(string), "fallback backend was never called")
}

func buildPDFMultipart(t *testing.T, content string) (io.Reader, string) {
	t.Helper()
	buf := &bytes.Buffer{}
	mw := multipart.NewWriter(buf)
	fw, err := docling.CreateFilePart(mw, "files", "test.pdf", "application/pdf")
	require.NoError(t, err)
	_, _ = fw.Write([]byte(content))
	require.NoError(t, mw.Close())
	return buf, mw.FormDataContentType()
}

// TestAuth_RouteByMIME_GenericOctetStreamSniffsAndRoutes is the
// regression for the user-reported bug: a client that uploads a PDF
// with the legacy / generic `application/octet-stream` Content-Type
// USED to fall through MIME-based routing entirely (the registry
// MIME allowlist looks for "application/pdf", not the generic
// token) — landing on the default engine, which then dispatched the
// request with the WRONG api_key and the backend returned 401.
//
// With mimedetect wired into convert.buildFileRequest, the proxy now
// sniffs the first 512 bytes of the spool, sees the %PDF-1.7 magic
// bytes, rewrites the effective MIME to application/pdf, and routes
// to the engine that actually owns PDFs.
func TestAuth_RouteByMIME_GenericOctetStreamSniffsAndRoutes(t *testing.T) {
	t.Parallel()

	var primaryHits, fallbackHits atomic.Int32
	var primarySeenKey atomic.Value
	primarySeenKey.Store("")

	primaryBE := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryHits.Add(1)
		primarySeenKey.Store(r.Header.Get("X-Api-Key"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"document":{"md_content":"primary ok"}}`))
	}))
	defer primaryBE.Close()

	fallbackBE := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"document":{"md_content":"fallback ok"}}`))
	}))
	defer fallbackBE.Close()

	mkAdapter := func(t *testing.T, name, url, key string, mimes []string) (engine.Engine, []string) {
		t.Helper()
		ec := config.EngineConfig{
			Enable:         true,
			CompatType:     config.CompatDocling,
			URL:            url,
			APIKey:         config.Secret(key),
			AuthHeader:     "X-Api-Key",
			AuthScheme:     config.AuthSchemeRaw,
			RequestTimeout: 5 * time.Second,
			MimeTypes:      mimes,
		}
		c, err := httpclient.New(ec)
		require.NoError(t, err)
		br := breaker.New(name, ec.Breaker, nil)
		ad, err := docling.New(engine.Name(name), ec, c, br)
		require.NoError(t, err)
		return ad, mimes
	}

	primaryEng, primaryMimes := mkAdapter(t, "primary", primaryBE.URL, "primary-key", []string{"application/pdf"})
	fallbackEng, _ := mkAdapter(t, "fallback", fallbackBE.URL, "fallback-key", nil)

	registry, err := engine.NewRegistry(map[engine.Name]engine.RegistryEntry{
		"primary":  {Engine: primaryEng, MimeTypes: primaryMimes},
		"fallback": {Engine: fallbackEng},
	}, "fallback", "")
	require.NoError(t, err)

	convert := &handlers.Convert{Registry: registry, Logger: zerolog.Nop()}
	front := httptest.NewServer(http.HandlerFunc(convert.File))
	defer front.Close()

	// The CRITICAL difference from TestAuth_RouteByMIME_NonDefault…:
	// the multipart part declares the generic application/octet-stream
	// instead of application/pdf. Without mimedetect the proxy would
	// fall through to "fallback" (default) and the test would fail.
	body, ctype := buildPDFMultipartWithCT(t, "%PDF-1.7\n%\xe2\xe3\xcf\xd3\nbody", "application/octet-stream")
	resp, err := http.Post(front.URL, ctype, body)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	require.EqualValues(t, 1, primaryHits.Load(),
		"sniffed application/pdf MUST route to the primary engine — without mimedetect this fell through to default and the auth header was wrong")
	require.EqualValues(t, 0, fallbackHits.Load())
	require.Equal(t, "primary-key", primarySeenKey.Load().(string),
		"the API key attached on the outbound MUST be the PRIMARY engine's, not the default's")
}

// buildPDFMultipartWithCT builds a multipart with an EXPLICIT
// per-part Content-Type — used to vary the declared MIME for the
// sniff-regression tests.
func buildPDFMultipartWithCT(t *testing.T, content, partCT string) (io.Reader, string) {
	t.Helper()
	buf := &bytes.Buffer{}
	mw := multipart.NewWriter(buf)
	fw, err := docling.CreateFilePart(mw, "files", "test.bin", partCT)
	require.NoError(t, err)
	_, _ = fw.Write([]byte(content))
	require.NoError(t, mw.Close())
	return buf, mw.FormDataContentType()
}
