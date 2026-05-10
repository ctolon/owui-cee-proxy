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

	rp, err := New(backend.URL, "/svc", "", "", nil, nil) // no trusted proxies
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
	rp, err := New(backend.URL, "/svc", "", "", nil, []string{"127.0.0.0/8"})
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
