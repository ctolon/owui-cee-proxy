// Package authutil is the single source of truth for outbound auth
// header stamping across every engine adapter. Before this package
// existed, three near-identical copies of the same logic lived in
// docling, external, and tika (inlined in tika.go). A regression in
// one (e.g., forgetting the bearer prefix) was invisible to the
// others, and the passthrough mount in reverseproxy/ had its own
// fourth implementation that silently dropped auth_scheme entirely.
//
// All four call sites now route through Apply. The function is
// intentionally tiny so the metrics callback and Outcome enum carry
// the value of the abstraction: every auth decision is observable.
package authutil

import (
	"net/http"

	"github.com/ctolon/owui-cee-proxy/internal/config"
)

// Outcome enumerates what Apply did with the call. Adapter call sites
// log this at debug level; the AuthMetrics implementation increments
// a Prometheus counter labelled by it.
type Outcome string

const (
	// OutcomeAttached: the configured header was stamped with a
	// non-empty value (key resolved + header name present).
	OutcomeAttached Outcome = "attached"
	// OutcomeMissingKey: auth_header is set but APIKey is empty. This
	// is the user-facing "I set DOCLING_API_KEY but the proxy isn't
	// forwarding it" failure mode — the YAML declared an auth
	// intention but the env var didn't propagate.
	OutcomeMissingKey Outcome = "missing_key"
	// OutcomeNoAuth: neither header nor key configured. The engine
	// is treated as public; no anomaly to surface.
	OutcomeNoAuth Outcome = "no_auth"
)

// AuthMetrics is the metrics seam. The production wiring binds it to
// the EngineAuthAttached Prometheus counter; tests pass a nop or
// recording double.
type AuthMetrics interface {
	// RecordAuthAttempt is called once per Apply invocation. engine
	// is the engines.<name> key; scheme is the configured auth_scheme
	// ("raw" | "bearer" | "" when no auth configured).
	RecordAuthAttempt(engine string, scheme string, outcome Outcome)
}

// Nop satisfies AuthMetrics with zero side effects. Useful for tests
// and for adapters that are constructed before the metrics registry
// is wired (e.g., contract tests).
type Nop struct{}

func (Nop) RecordAuthAttempt(string, string, Outcome) {}

// Apply stamps cfg.AuthHeader on h with cfg.APIKey, honouring
// cfg.AuthScheme. It returns the Outcome so the caller can log/inspect
// the decision, AND it reports to metrics in the same call. Callers
// MUST NOT inspect cfg.APIKey directly — go through Apply so the
// metrics counter stays the source of truth.
//
// Threading: safe — h is mutated, but the caller is responsible for
// not sharing h across goroutines.
func Apply(h http.Header, engineName string, cfg config.EngineConfig, m AuthMetrics) Outcome {
	scheme := string(cfg.AuthScheme)
	if cfg.AuthHeader == "" && cfg.APIKey == "" {
		m.RecordAuthAttempt(engineName, scheme, OutcomeNoAuth)
		return OutcomeNoAuth
	}
	if cfg.APIKey == "" {
		m.RecordAuthAttempt(engineName, scheme, OutcomeMissingKey)
		return OutcomeMissingKey
	}
	if cfg.AuthHeader == "" {
		// Key without a header to put it on — same logical outcome
		// as "missing key" from an auth-attempt standpoint, but we
		// label it distinctly so a misconfiguration is easier to
		// spot in the dashboard.
		m.RecordAuthAttempt(engineName, scheme, OutcomeMissingKey)
		return OutcomeMissingKey
	}

	value := string(cfg.APIKey)
	if cfg.AuthScheme == config.AuthSchemeBearer {
		value = "Bearer " + value
	}
	h.Set(cfg.AuthHeader, value)
	m.RecordAuthAttempt(engineName, scheme, OutcomeAttached)
	return OutcomeAttached
}

// FormatHeaderValue renders the wire-format value Apply would set,
// WITHOUT mutating any header. Used by the reverseproxy passthrough
// mount which doesn't have an http.Header at construction time but
// needs to know what to inject inside its Rewrite closure.
//
// Returns "" when no auth would be stamped (caller should NOT set
// the header). Never returns "Bearer " (with a trailing space) for
// an empty key — that's the silent-failure mode the proxy is
// explicitly designed to avoid.
func FormatHeaderValue(cfg config.EngineConfig) string {
	if cfg.AuthHeader == "" || cfg.APIKey == "" {
		return ""
	}
	if cfg.AuthScheme == config.AuthSchemeBearer {
		return "Bearer " + string(cfg.APIKey)
	}
	return string(cfg.APIKey)
}
