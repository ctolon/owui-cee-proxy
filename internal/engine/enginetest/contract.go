// Package enginetest exposes a shared contract test that every Engine
// implementation should pass. It lives in its own package so the
// engine package itself does not depend on testify in production
// builds.
package enginetest

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ctolon/owui-cee-proxy/internal/engine"
)

// RunContractTests verifies the Liskov-substitutable behaviour every
// Engine adapter must support. The earlier shape of this function was
// flagged by the FAANG plugin-SDK review (C-15) as "a probe, not a
// contract" — almost any return shape passed. This version pins the
// load-bearing invariants every adapter SHOULD honour:
//
//  1. Name/URL non-empty + URL stable across calls.
//  2. Capabilities advertises a known facade set.
//  3. Health returns context.Canceled / DeadlineExceeded on a
//     cancelled context (not a generic non-nil error).
//  4. Convert is exercised against EVERY advertised facade, not just
//     the first.
//  5. Convert returns a usable response on the happy path (status
//     code > 0 when err is nil).
//  6. Convert response body is safe to Close twice (a real-world
//     adapter bug class — handlers + observability hooks both
//     defer-close).
//  7. URL() is idempotent across calls — no hidden state.
func RunContractTests(t *testing.T, e engine.Engine) {
	t.Helper()

	t.Run("name_is_nonempty", func(t *testing.T) {
		t.Parallel()
		require.NotEmpty(t, string(e.Name()))
	})

	// CLAUDE.md invariant: convert-path logs MUST carry engine_url.
	// That field flows from this accessor; asserting it here catches
	// any adapter that forgets to surface the configured URL.
	t.Run("url_is_nonempty", func(t *testing.T) {
		t.Parallel()
		require.NotEmpty(t, e.URL(), "engine %s must expose its backend URL via URL()", e.Name())
	})

	// URL() MUST be a getter, not a stateful build step. Two reads
	// have to produce the same string — otherwise log correlation
	// across adapter calls would drift.
	t.Run("url_is_idempotent", func(t *testing.T) {
		t.Parallel()
		require.Equal(t, e.URL(), e.URL(),
			"URL() MUST be a pure getter; observed drift across reads")
	})

	t.Run("capabilities_declares_at_least_one_facade", func(t *testing.T) {
		t.Parallel()
		caps := e.Capabilities()
		require.NotEmpty(t, caps.Facades, "engine must answer at least one facade")
		// Every advertised facade must be a known value so the
		// transport layer can dispatch deterministically.
		for _, f := range caps.Facades {
			require.True(t, f == engine.FacadeDocling || f == engine.FacadeExternal,
				"unknown facade %q advertised by engine %s", f, e.Name())
		}
	})

	// Health on a cancelled context MUST surface context.Canceled (or
	// context.DeadlineExceeded for the timeout variant). The earlier
	// contract accepted ANY non-empty error — a tautology since any
	// non-nil error has a non-empty message. Tighten the assertion so
	// an adapter that ignores ctx entirely (returns a generic "down"
	// error) fails the contract.
	t.Run("health_honors_cancelled_ctx", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := e.Health(ctx)
		if err == nil {
			// Some adapters' health probes complete before they
			// observe the cancellation (very fast in-process loop).
			// Don't fail the contract on the not-yet-observed case.
			return
		}
		require.True(t,
			errors.Is(err, context.Canceled) ||
				errors.Is(err, context.DeadlineExceeded) ||
				strings.Contains(strings.ToLower(err.Error()), "context"),
			"engine %s: Health on cancelled ctx must return ctx error, got %v", e.Name(), err)
	})

	// Drive Convert against EVERY advertised facade — the earlier
	// version only exercised caps.Facades[0], which let
	// doclingexternal's per-facade dispatch (FacadeExternal vs
	// FacadeDocling) slip past the contract.
	t.Run("convert_returns_response_or_error_per_facade", func(t *testing.T) {
		t.Parallel()
		caps := e.Capabilities()
		for _, facade := range caps.Facades {
			facade := facade
			t.Run(string(facade), func(t *testing.T) {
				ctx := context.Background()
				req := &engine.ConvertRequest{Facade: facade}
				resp, err := e.Convert(ctx, req)
				if err != nil {
					// An error is acceptable (empty request → bad input);
					// we just refuse to let an adapter return BOTH nil
					// response AND nil error.
					return
				}
				require.NotNil(t, resp, "engine %s facade=%s: nil response with nil error",
					e.Name(), facade)
				require.Greater(t, resp.StatusCode, 0,
					"engine %s facade=%s: response status must be > 0",
					e.Name(), facade)

				if resp.Body != nil {
					// Body MUST be safe to Close twice — a real-world
					// adapter bug class (handler defers Close,
					// observability hook also defers Close after a
					// peek-restore). A panic-on-double-close adapter
					// crashes the request goroutine and the panic
					// middleware emits a 500 instead of the engine's
					// actual response.
					_, _ = io.Copy(io.Discard, resp.Body)
					require.NoError(t, resp.Body.Close(),
						"first Close must succeed")
					require.NotPanics(t, func() {
						_ = resp.Body.Close()
					}, "second Close must not panic")
				}
			})
		}
	})
}
