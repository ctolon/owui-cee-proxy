package breaker_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ctolon/owui-cee-proxy/internal/breaker"
	"github.com/ctolon/owui-cee-proxy/internal/config"
)

func TestStateValue_KnownStates(t *testing.T) {
	t.Parallel()
	require.Equal(t, float64(0), breaker.StateValue("closed"))
	require.Equal(t, float64(1), breaker.StateValue("half-open"))
	require.Equal(t, float64(2), breaker.StateValue("open"))
	require.Equal(t, float64(-1), breaker.StateValue("anything-else"),
		"unknown states map to -1 so the gauge can be alerted on")
}

// TestNew_OnStateChangeFiresOnTrip pins the integration with
// sony/gobreaker: consecutive failures past the configured threshold
// must trigger the OnStateChange callback with "closed → open".
// This is the only mechanism by which BreakerStateGauge ever moves;
// without this test a refactor that drops the StateChange wrapper
// would silently freeze the gauge at 0.
func TestNew_OnStateChangeFiresOnTrip(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	type call struct{ name, from, to string }
	var calls []call

	b := breaker.New("test", config.BreakerConfig{
		MaxRequests:                  1,
		Interval:                     time.Minute,
		Timeout:                      time.Minute,
		ConsecutiveFailuresThreshold: 2,
	}, func(name, from, to string) {
		mu.Lock()
		calls = append(calls, call{name, from, to})
		mu.Unlock()
	})

	// Drive 2 consecutive failures → breaker should trip closed→open.
	for i := 0; i < 2; i++ {
		_, _ = b.Execute(func() (any, error) {
			return nil, errors.New("boom")
		})
	}

	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, calls, "OnStateChange callback must fire after threshold breaches")
	last := calls[len(calls)-1]
	require.Equal(t, "test", last.name)
	require.Equal(t, "open", last.to,
		"breaker must be open after consecutive failures past threshold")
}

// TestNew_NilCallbackIsSafe documents the test/integration path:
// when no observability hook is wired, breaker.New still works and
// gobreaker's behaviour is unchanged.
func TestNew_NilCallbackIsSafe(t *testing.T) {
	t.Parallel()
	require.NotPanics(t, func() {
		b := breaker.New("nop", config.BreakerConfig{
			MaxRequests:                  1,
			Interval:                     time.Minute,
			Timeout:                      time.Minute,
			ConsecutiveFailuresThreshold: 1,
		}, nil)
		_, _ = b.Execute(func() (any, error) { return "ok", nil })
	})
}
