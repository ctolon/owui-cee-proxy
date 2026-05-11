// Package breaker wraps sony/gobreaker with the project's config shape.
package breaker

import (
	"github.com/sony/gobreaker"

	"github.com/ctolon/owui-cee-proxy/internal/config"
)

type Breaker = gobreaker.CircuitBreaker

// StateChangeFunc is the observability hook invoked whenever a
// breaker's gobreaker state machine transitions. The arguments are
// the breaker's name and the from/to state names ("closed",
// "half-open", "open") — already-stringified for direct logging.
//
// Bind this to the Prometheus BreakerStateGauge AND emit an info log
// line ("breaker_state_changed") at the composition root; do not
// inline metric writes inside this package, so internal/breaker
// stays observability-package-free.
type StateChangeFunc func(name, from, to string)

// New constructs a breaker pointed at the engines.<name> entry. When
// onChange is non-nil it fires for every state transition; pass nil
// in tests that don't care about observability.
func New(name string, cfg config.BreakerConfig, onChange StateChangeFunc) *Breaker {
	settings := gobreaker.Settings{
		Name:        name,
		MaxRequests: cfg.MaxRequests,
		Interval:    cfg.Interval,
		Timeout:     cfg.Timeout,
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			return counts.ConsecutiveFailures >= cfg.ConsecutiveFailuresThreshold
		},
	}
	if onChange != nil {
		settings.OnStateChange = func(brName string, from, to gobreaker.State) {
			onChange(brName, from.String(), to.String())
		}
	}
	return gobreaker.NewCircuitBreaker(settings)
}

// StateValue maps a gobreaker state string to the numeric gauge
// encoding the project uses across dashboards.
//
//	closed   = 0
//	half-open = 1
//	open     = 2
//
// Returns -1 for unknown values so an operator alerts on
// breaker_state == -1 (means an upstream library upgrade introduced
// a new state we don't yet handle).
func StateValue(state string) float64 {
	switch state {
	case "closed":
		return 0
	case "half-open":
		return 1
	case "open":
		return 2
	default:
		return -1
	}
}
