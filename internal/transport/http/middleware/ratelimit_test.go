package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestGlobalRateLimit_DisabledIsNoop(t *testing.T) {
	t.Parallel()
	calls := 0
	handler := GlobalRateLimit(0, 0)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
	}))
	for i := 0; i < 10; i++ {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	}
	if calls != 10 {
		t.Fatalf("disabled limiter dropped requests: %d/10", calls)
	}
}

func TestGlobalRateLimit_AllowsWithinBurst(t *testing.T) {
	t.Parallel()
	// Tiny RPS so refills don't muddy the burst budget during the test;
	// burst=3 is the only allowance we expect to see.
	handler := GlobalRateLimit(0.1, 3)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	for i := 0; i < 3; i++ {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("call %d: want 200 got %d", i, w.Code)
		}
	}
}

func TestGlobalRateLimit_RejectsBeyondBurst(t *testing.T) {
	t.Parallel()
	handler := GlobalRateLimit(0.1, 1)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	w1 := httptest.NewRecorder()
	handler.ServeHTTP(w1, httptest.NewRequest(http.MethodGet, "/", nil))
	if w1.Code != http.StatusOK {
		t.Fatalf("first call: want 200 got %d", w1.Code)
	}

	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, httptest.NewRequest(http.MethodGet, "/", nil))
	if w2.Code != http.StatusTooManyRequests {
		t.Fatalf("second call: want 429 got %d", w2.Code)
	}
}

func TestGlobalRateLimit_RefillsAfterInterval(t *testing.T) {
	t.Parallel()
	// 200 RPS burst=1: a 30ms wait yields ~6 tokens worth of refill,
	// well over the single slot we exhaust. Cushion keeps the test
	// reliable on a busy CI runner.
	handler := GlobalRateLimit(200, 1)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	w1 := httptest.NewRecorder()
	handler.ServeHTTP(w1, httptest.NewRequest(http.MethodGet, "/", nil))
	if w1.Code != http.StatusOK {
		t.Fatalf("first: %d", w1.Code)
	}

	time.Sleep(30 * time.Millisecond)

	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, httptest.NewRequest(http.MethodGet, "/", nil))
	if w2.Code != http.StatusOK {
		t.Fatalf("after refill: want 200 got %d", w2.Code)
	}
}
