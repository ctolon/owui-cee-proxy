package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAPIKey_DisabledIsNoOp(t *testing.T) {
	t.Parallel()
	called := false
	mw := APIKey("X-Proxy-Api-Key", nil, false)
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	h.ServeHTTP(rec, req)

	require.True(t, called)
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestAPIKey_RequiredEmptyAllowlistRejects(t *testing.T) {
	t.Parallel()
	// L1 corner case: required=true but allowlist is empty (likely a
	// misconfiguration). The middleware should fail closed.
	mw := APIKey("X-Proxy-Api-Key", []string{}, true)
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be reached")
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Proxy-Api-Key", "anything")
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestAPIKey_MissingHeaderRejects(t *testing.T) {
	t.Parallel()
	mw := APIKey("X-Proxy-Api-Key", []string{"secret-key"}, true)
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be reached")
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestAPIKey_WrongKeyRejects(t *testing.T) {
	t.Parallel()
	mw := APIKey("X-Proxy-Api-Key", []string{"correct-key"}, true)
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be reached")
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Proxy-Api-Key", "wrong-key")
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestAPIKey_CorrectKeyAccepts(t *testing.T) {
	t.Parallel()
	called := false
	mw := APIKey("X-Proxy-Api-Key", []string{"k1", "k2", "secret"}, true)
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Proxy-Api-Key", "secret")
	h.ServeHTTP(rec, req)

	require.True(t, called)
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestAPIKey_DifferentLengthKeysDoNotPanic(t *testing.T) {
	t.Parallel()
	// Pre-L1, comparing keys of different lengths via
	// subtle.ConstantTimeCompare returned 0 immediately and leaked
	// the length. Post-L1 both sides are SHA-256ed first, so a
	// supplied key of any length is hashed to 32 bytes and compared
	// at constant length. The behavior we assert here is "rejection
	// without crash".
	mw := APIKey("X-Proxy-Api-Key", []string{"short"}, true)
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be reached")
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Proxy-Api-Key", "this-is-a-much-longer-string")
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnauthorized, rec.Code)
}
