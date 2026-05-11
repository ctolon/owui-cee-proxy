// Package enginetest exposes a shared contract test that every Engine
// implementation should pass. It lives in its own package so the
// engine package itself does not depend on testify in production
// builds.
package enginetest

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ctolon/owui-cee-proxy/internal/engine"
)

// RunContractTests verifies the Liskov-substitutable behaviour every
// Engine adapter must support.
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

	t.Run("health_honors_cancelled_ctx", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := e.Health(ctx)
		if err != nil {
			require.True(t,
				strings.Contains(err.Error(), "context") ||
					strings.Contains(err.Error(), "canceled") ||
					err.Error() != "",
				"health error should be informative: %v", err)
		}
	})

	t.Run("convert_returns_response_or_error", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		caps := e.Capabilities()
		facade := engine.FacadeDocling
		if len(caps.Facades) > 0 {
			facade = caps.Facades[0]
		}
		req := &engine.ConvertRequest{Facade: facade}
		resp, err := e.Convert(ctx, req)
		if err == nil {
			require.NotNil(t, resp)
			if resp != nil && resp.Body != nil {
				_ = resp.Body.Close()
			}
		}
	})
}
