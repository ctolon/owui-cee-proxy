package app

import (
	"github.com/rs/zerolog"

	"github.com/ctolon/owui-cee-proxy/internal/breaker"
	"github.com/ctolon/owui-cee-proxy/internal/config"
	"github.com/ctolon/owui-cee-proxy/internal/engine"
	"github.com/ctolon/owui-cee-proxy/internal/observability"
)

// logEngineBootstrap emits one info line per enabled engine + one warn
// per non-fatal misconfiguration discovered at load time. Operators see
// these in `kubectl logs` immediately after a rollout; together they
// answer "did my Secret reach the Pod?" without per-request log
// archaeology.
func logEngineBootstrap(logger zerolog.Logger, cfg *config.Config) {
	for name, ec := range cfg.Engines {
		if !ec.Enable {
			continue
		}
		facades := config.AcceptedFacades(ec.CompatType)
		logger.Info().
			Str("event", "engine_initialised").
			Str("engine", name).
			Str("compat_type", string(ec.CompatType)).
			Str("url", ec.URL).
			Str("api_key_env", ec.APIKeyEnv).
			Bool("api_key_resolved", ec.APIKey != "").
			Int("api_key_len", len(string(ec.APIKey))).
			Str("auth_header", ec.AuthHeader).
			Str("auth_scheme", string(ec.AuthScheme)).
			Strs("facades", facades).
			Int("mime_types_count", len(ec.MimeTypes)).
			Int("extensions_count", len(ec.Extensions)).
			Msg("engine initialised")
	}
	for _, w := range cfg.AuthWarnings() {
		logger.Warn().
			Str("event", "engine_auth_warning").
			Msg(w)
	}
}

// logRoutingBootstrap emits a single record describing the effective
// routing strategy + the number of operator-supplied MIME extension
// overrides. Surface so operators can verify the YAML knobs landed
// without diffing a logback config.
func logRoutingBootstrap(logger zerolog.Logger, cfg *config.Config, reg engine.Registry, overrideCount int) {
	logger.Info().
		Str("event", "routing_strategy_initialised").
		Str("configured_strategy", string(cfg.Routing.Strategy)).
		Str("effective_strategy", string(reg.Strategy())).
		Str("default_engine", cfg.Routing.DefaultEngine).
		Int("mime_extension_overrides_count", overrideCount).
		Msg("routing strategy initialised")
}

// breakerStateHook returns the OnStateChange callback bound to the
// project's Prometheus gauge + structured logger. Lives at the
// composition root so internal/breaker stays observability-agnostic.
//
// The hook is purely additive: it does NOT influence the breaker's
// state machine, only surfaces transitions to operators. Logged at
// info — half-open → open transitions warrant an alert dashboard,
// not a paging line.
func breakerStateHook(logger zerolog.Logger, metrics *observability.Metrics) breaker.StateChangeFunc {
	return func(name, from, to string) {
		if metrics != nil && metrics.BreakerStateGauge != nil {
			metrics.BreakerStateGauge.WithLabelValues(name).Set(breaker.StateValue(to))
		}
		logger.Info().
			Str("event", "breaker_state_changed").
			Str("engine", name).
			Str("from", from).
			Str("to", to).
			Msg("circuit breaker state changed")
	}
}
