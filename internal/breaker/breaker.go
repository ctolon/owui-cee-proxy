// Package breaker wraps sony/gobreaker with the project's config shape.
package breaker

import (
	"github.com/sony/gobreaker"

	"github.com/ctolon/owui-cee-proxy/internal/config"
)

type Breaker = gobreaker.CircuitBreaker

func New(name string, cfg config.BreakerConfig) *Breaker {
	settings := gobreaker.Settings{
		Name:        name,
		MaxRequests: cfg.MaxRequests,
		Interval:    cfg.Interval,
		Timeout:     cfg.Timeout,
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			return counts.ConsecutiveFailures >= cfg.ConsecutiveFailuresThreshold
		},
	}
	return gobreaker.NewCircuitBreaker(settings)
}
