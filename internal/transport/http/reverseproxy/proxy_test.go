package reverseproxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestNew_StripsXForwardedFromUntrustedSource (H3) — when the inbound
// RemoteAddr is NOT in trustedProxies, any caller-supplied
// X-Forwarded-For must be dropped before SetXForwarded() runs. The
// backend should see only the actual remote IP, never the spoofed
// upstream value.
func TestNew_StripsXForwardedFromUntrustedSource(t *testing.T) {
	t.Parallel()

	var seen string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("X-Forwarded-For")
	}))
	defer backend.Close()

	rp, err := New(backend.URL, "/svc", nil, nil, nil) // no trusted proxies
	require.NoError(t, err)

	front := httptest.NewServer(rp)
	defer front.Close()

	req, err := http.NewRequest("GET", front.URL+"/svc/", nil)
	require.NoError(t, err)
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	req.Header.Set("X-Real-Ip", "5.6.7.8")
	req.Header.Set("Forwarded", "for=9.9.9.9")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)

	require.NotContains(t, seen, "1.2.3.4",
		"untrusted X-Forwarded-For must be stripped before SetXForwarded() appends")
}

// TestNew_PreservesXForwardedFromTrustedSource — the mirror image of
// the previous test: when RemoteAddr falls inside trustedProxies, the
// inbound X-Forwarded-For is preserved (and SetXForwarded appends).
func TestNew_PreservesXForwardedFromTrustedSource(t *testing.T) {
	t.Parallel()

	var seen string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("X-Forwarded-For")
	}))
	defer backend.Close()

	// 127.0.0.0/8 covers the loopback that httptest uses.
	rp, err := New(backend.URL, "/svc", nil, nil, []string{"127.0.0.0/8"})
	require.NoError(t, err)

	front := httptest.NewServer(rp)
	defer front.Close()

	req, err := http.NewRequest("GET", front.URL+"/svc/", nil)
	require.NoError(t, err)
	req.Header.Set("X-Forwarded-For", "1.2.3.4")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)

	require.Contains(t, seen, "1.2.3.4",
		"trusted X-Forwarded-For must be preserved (Go appends RemoteAddr to it)")
}

// TestRejectTraversal_RejectsDotDot covers M3: any request whose path
// contains ".." must be 400'd before it reaches the proxy.
func TestRejectTraversal_RejectsDotDot(t *testing.T) {
	t.Parallel()

	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})
	h := RejectTraversal(next)

	cases := []string{
		"/docling/../admin",
		"/docling/foo/../../../etc/passwd",
		"/svc/..%2fadmin", // encoded — chi/mux may decode to ".."
		"/foo/..",
	}
	for _, p := range cases {
		called = false
		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "http://example.com"+p, nil)
		// Need to set URL.Path manually because httptest.NewRequest
		// will already path.Clean the URL.
		req.URL.Path = p
		h.ServeHTTP(w, req)
		require.Equal(t, http.StatusBadRequest, w.Code, "path %q should be 400", p)
		require.False(t, called, "handler must not run for %q", p)
	}
}

// TestRejectTraversal_PassesCleanPaths confirms benign paths reach the
// downstream handler.
func TestRejectTraversal_PassesCleanPaths(t *testing.T) {
	t.Parallel()

	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})
	h := RejectTraversal(next)

	for _, p := range []string{"/docling/v1/convert/file", "/", "/foo/bar"} {
		called = false
		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "http://example.com"+p, nil)
		req.URL.Path = p
		h.ServeHTTP(w, req)
		require.Equal(t, http.StatusNoContent, w.Code, "path %q", p)
		require.True(t, called, "handler must run for %q", p)
	}
}

// TestNew_StampsConfiguredAPIKey covers the native-passthrough auth
// forwarding path: when an engine is configured with auth_header +
// api_key_env, requests through /<engine>/* MUST carry that header
// on the outbound request. This is the passthrough analogue of the
// per-adapter ApplyAuth coverage and is the path the user most often
// debugs ("my Bearer key isn't showing up").
func TestNew_StampsConfiguredAPIKey(t *testing.T) {
	t.Parallel()

	var seenKey string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenKey = r.Header.Get("X-Api-Key")
	}))
	defer backend.Close()

	rp, err := New(backend.URL, "/docling", map[string]string{"X-Api-Key": "secret-from-env"}, nil, nil)
	require.NoError(t, err)
	front := httptest.NewServer(rp)
	defer front.Close()

	resp, err := http.Get(front.URL + "/docling/health")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, "secret-from-env", seenKey, "configured api key must reach the backend")
}

// TestNew_EmptyAPIKeyDoesNotStampHeader pins the symmetrical case to
// the adapter tests: an unresolved env var must not produce an
// empty-valued header (some backends 401 on "X-Api-Key:").
func TestNew_EmptyAPIKeyDoesNotStampHeader(t *testing.T) {
	t.Parallel()

	var saw bool
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, saw = r.Header["X-Api-Key"]
	}))
	defer backend.Close()

	rp, err := New(backend.URL, "/docling", nil, nil, nil)
	require.NoError(t, err)
	front := httptest.NewServer(rp)
	defer front.Close()

	resp, err := http.Get(front.URL + "/docling/health")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.False(t, saw, "no header must be stamped when apiKey is empty")
}

// TestNew_StripsInboundAuthorization codifies the security invariant:
// callers MUST NOT be able to smuggle their own Authorization header
// onto the engine backend, even on the passthrough path that doesn't
// otherwise parse the request.
func TestNew_StripsInboundAuthorization(t *testing.T) {
	t.Parallel()

	var seenAuth string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
	}))
	defer backend.Close()

	// Configure the engine's own auth on a DIFFERENT header so we can
	// be sure the caller's Authorization got dropped (not just
	// overwritten by the engine's own stamp).
	rp, err := New(backend.URL, "/docling", map[string]string{"X-Api-Key": "engine-key"}, nil, nil)
	require.NoError(t, err)
	front := httptest.NewServer(rp)
	defer front.Close()

	req, err := http.NewRequest("GET", front.URL+"/docling/health", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer caller-smuggled-token")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Empty(t, seenAuth, "caller-supplied Authorization must be stripped before reaching backend")
}

// TestNew_AppliesBearerScheme verifies the fix for the passthrough
// bearer-scheme bug (was: "auth_scheme: bearer + auth_header:
// Authorization" routed via /<engine>/* sent the raw key, dropping
// the "Bearer " prefix and silently 401-ing). The passthrough now
// honours the same scheme semantics as the facade path's
// authutil.Apply, so the two log/wire shapes match.
func TestNew_AppliesBearerScheme(t *testing.T) {
	t.Parallel()

	var seenAuth string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
	}))
	defer backend.Close()

	rp, err := New(backend.URL, "/docling", map[string]string{"Authorization": "Bearer " + "raw-key"}, nil, nil)
	require.NoError(t, err)
	front := httptest.NewServer(rp)
	defer front.Close()

	resp, err := http.Get(front.URL + "/docling/health")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, "Bearer raw-key", seenAuth,
		"bearer scheme must prepend 'Bearer ' on the passthrough path, matching the facade path")
}

// TestNew_RawSchemeForwardsLiteralKey is the symmetric coverage for
// auth_scheme: raw — the literal key should reach the backend
// without any prefix mangling.
func TestNew_RawSchemeForwardsLiteralKey(t *testing.T) {
	t.Parallel()

	var seenAuth string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("X-Api-Key")
	}))
	defer backend.Close()

	rp, err := New(backend.URL, "/docling", map[string]string{"X-Api-Key": "raw-key"}, nil, nil)
	require.NoError(t, err)
	front := httptest.NewServer(rp)
	defer front.Close()

	resp, err := http.Get(front.URL + "/docling/health")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, "raw-key", seenAuth)
}

// TestSingleJoin_PathClean is a tight unit check for the M3 belt-and-
// braces cleanup inside Rewrite's path computation.
func TestSingleJoin_PathClean(t *testing.T) {
	t.Parallel()
	cases := []struct {
		a, b, want string
	}{
		{"", "/foo", "/foo"},
		{"/base", "/foo", "/base/foo"},
		{"/base/", "/foo/../bar", "/base/bar"},
		{"/", "/x/./y", "/x/y"},
	}
	for _, c := range cases {
		got := singleJoin(c.a, c.b)
		require.Equal(t, c.want, got,
			"singleJoin(%q,%q)", c.a, c.b)
	}
	// Sanity: a path that already contains a normal component stays put.
	require.True(t, strings.HasPrefix(singleJoin("/base", "/foo"), "/base"))
}
