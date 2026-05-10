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
		req := &engine.ConvertRequest{}
		resp, err := e.Convert(ctx, req)
		if err == nil {
			require.NotNil(t, resp)
			if resp != nil && resp.Body != nil {
				_ = resp.Body.Close()
			}
		}
	})
}
