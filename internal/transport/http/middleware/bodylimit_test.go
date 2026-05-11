package middleware

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBodyLimit_UnderCapPassesThrough(t *testing.T) {
	t.Parallel()
	const max = 1024
	var got []byte
	handler := BodyLimit(max)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		got = b
		w.WriteHeader(http.StatusOK)
	}))
	body := bytes.Repeat([]byte("a"), 512)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200 got %d", w.Code)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("body bytes lost in flight: got %d want %d", len(got), len(body))
	}
}

func TestBodyLimit_OverCapReturns413(t *testing.T) {
	t.Parallel()
	const max = 64
	var readErr error
	handler := BodyLimit(max)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, readErr = io.ReadAll(r.Body)
		if readErr != nil {
			// MaxBytesReader writes the 413 itself; mirror its behaviour.
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		}
	}))
	body := bytes.Repeat([]byte("a"), int(max)+1)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if readErr == nil {
		t.Fatal("expected MaxBytesReader to error past the cap")
	}
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status: want 413 got %d", w.Code)
	}
}

func TestBodyLimit_DisabledIsNoop(t *testing.T) {
	t.Parallel()
	var bodyLen int
	handler := BodyLimit(0)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		bodyLen = len(b)
	}))
	// Larger than any positive cap to confirm the disabled path
	// really runs the request through.
	body := bytes.Repeat([]byte("x"), 1<<20)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if bodyLen != len(body) {
		t.Fatalf("got %d bytes, want %d", bodyLen, len(body))
	}
}

func TestBodyBytesIn_ReturnsRunningCount(t *testing.T) {
	t.Parallel()
	var snapshot int64
	handler := BodyLimit(1024)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			t.Fatalf("read: %v", err)
		}
		snapshot = bodyBytesIn(r.Context())
	}))
	body := strings.Repeat("z", 100)
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if snapshot != int64(len(body)) {
		t.Fatalf("counter: got %d want %d", snapshot, len(body))
	}
}

func TestBodyBytesIn_ZeroWithoutMiddleware(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if n := bodyBytesIn(req.Context()); n != 0 {
		t.Fatalf("uninstalled counter: got %d want 0", n)
	}
}
