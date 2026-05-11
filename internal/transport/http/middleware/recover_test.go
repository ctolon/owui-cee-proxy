package middleware

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"
)

type recordingPanicRecorder struct {
	paths []string
}

func (r *recordingPanicRecorder) RecordPanic(path string) {
	r.paths = append(r.paths, path)
}

func TestRecover_CatchesPanicReturns500(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := zerolog.New(&buf)
	rec := &recordingPanicRecorder{}
	handler := Recover(logger, rec)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/convert/file", nil))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status: want 500 got %d", w.Code)
	}
	if !bytes.Contains(buf.Bytes(), []byte("panic_recovered")) {
		t.Fatalf("expected panic_recovered event in log, got: %s", buf.String())
	}
	if !bytes.Contains(buf.Bytes(), []byte("\"path\":\"/v1/convert/file\"")) {
		t.Fatalf("path field missing from log line: %s", buf.String())
	}
	// C-39: stack must be a STRING field, not Bytes — Bytes would
	// base64-encode it, making the log line unreadable.
	if !bytes.Contains(buf.Bytes(), []byte("\"stack\":\"goroutine")) {
		t.Fatalf("expected stack to begin with `\"stack\":\"goroutine`, got: %s", buf.String())
	}
	if len(rec.paths) != 1 || rec.paths[0] != "/v1/convert/file" {
		t.Fatalf("recorder calls: %v", rec.paths)
	}
}

func TestRecover_NilRecorderUsesNopFallback(t *testing.T) {
	t.Parallel()
	// nil recorder must NOT panic; the package wires the no-op default.
	handler := Recover(zerolog.New(io.Discard), nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status: want 500 got %d", w.Code)
	}
}

func TestRecover_NormalRequestUnchanged(t *testing.T) {
	t.Parallel()
	rec := &recordingPanicRecorder{}
	handler := Recover(zerolog.New(io.Discard), rec)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	if w.Code != http.StatusTeapot {
		t.Fatalf("non-panic path: want 418 got %d", w.Code)
	}
	if len(rec.paths) != 0 {
		t.Fatalf("recorder fired on non-panic path: %v", rec.paths)
	}
}

func TestRecover_OmitNoRecorderArgWorks(t *testing.T) {
	t.Parallel()
	// Variadic form: legacy callers passed no recorder; still safe.
	handler := Recover(zerolog.New(io.Discard))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("legacy")
	}))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status: want 500 got %d", w.Code)
	}
}
