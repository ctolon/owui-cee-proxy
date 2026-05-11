package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTimeout_DeadlinePropagates(t *testing.T) {
	t.Parallel()
	var captured context.Context
	handler := Timeout(50 * time.Millisecond)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Context()
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if captured == nil {
		t.Fatal("handler did not run")
	}
	if _, ok := captured.Deadline(); !ok {
		t.Fatal("expected context.WithTimeout to set a deadline")
	}
}

func TestTimeout_FiresAfterDuration(t *testing.T) {
	t.Parallel()
	var ctxErr error
	handler := Timeout(20 * time.Millisecond)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		ctxErr = r.Context().Err()
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if !errors.Is(ctxErr, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded, got %v", ctxErr)
	}
}

func TestTimeout_DisabledIsNoop(t *testing.T) {
	t.Parallel()
	var hasDeadline bool
	handler := Timeout(0)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hasDeadline = r.Context().Deadline()
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if hasDeadline {
		t.Fatal("disabled Timeout should not set a deadline")
	}
}
