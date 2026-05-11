package handlers

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCopyResponseHeaders_AllowlistStripsServerAndSetCookie pins
// the C-30 fix: upstream `Server`, `X-Powered-By`, `Set-Cookie`,
// internal correlation IDs MUST NOT cross the proxy boundary.
// Anything not on outboundResponseHeaderAllowlist is dropped; the
// always-dropped set is a belt-and-braces override.
func TestCopyResponseHeaders_AllowlistStripsServerAndSetCookie(t *testing.T) {
	t.Parallel()
	src := http.Header{}
	src.Set("Content-Type", "application/json")
	src.Set("Content-Length", "42")
	src.Set("ETag", `W/"abc"`)
	src.Set("Server", "engine-internal/1.2.3")
	src.Set("X-Powered-By", "internal-stack")
	src.Set("Set-Cookie", "session=do-not-leak")
	src.Set("X-Internal-Trace", "leaked-debug-id")
	src.Add("Set-Cookie", "another=leak")

	dst := http.Header{}
	copyResponseHeaders(dst, src)

	require.Equal(t, "application/json", dst.Get("Content-Type"))
	require.Equal(t, "42", dst.Get("Content-Length"))
	require.Equal(t, `W/"abc"`, dst.Get("ETag"))
	require.Empty(t, dst.Get("Server"), "Server MUST be stripped")
	require.Empty(t, dst.Get("X-Powered-By"), "X-Powered-By MUST be stripped")
	require.Empty(t, dst.Values("Set-Cookie"), "Set-Cookie MUST be stripped (both values)")
	require.Empty(t, dst.Get("X-Internal-Trace"),
		"unknown header MUST be dropped by the allowlist filter")
}

// TestCopyResponseHeaders_RejectsCRLFInValues — even a header on the
// allowlist must drop values containing CR/LF, defending against a
// compromised backend that smuggles a header-injection payload as
// the body of a "Content-Type" value.
func TestCopyResponseHeaders_RejectsCRLFInValues(t *testing.T) {
	t.Parallel()
	src := http.Header{
		"Content-Type": []string{"application/json\r\nSet-Cookie: leaked"},
	}
	dst := http.Header{}
	copyResponseHeaders(dst, src)
	require.Empty(t, dst.Get("Content-Type"),
		"CR/LF-bearing value MUST be dropped even from allowlisted headers")
	require.Empty(t, dst.Get("Set-Cookie"))
}
