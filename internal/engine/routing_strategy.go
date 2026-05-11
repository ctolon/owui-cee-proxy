package engine

// RoutingStrategy controls how the registry picks an engine for a
// given facade + (mime, filename) request hint. Declared as a typed
// string so misspelled switch arms get caught at compile time.
//
// The config package re-exports this type via a Go type alias
// (`type RoutingStrategy = engine.RoutingStrategy`) so the YAML
// validator + handlers can refer to it without the engine package
// importing config (CLAUDE.md layering invariant).
type RoutingStrategy string

// Routing strategies.
//
//	StrategyMime         — pick by MIME allowlist only (v0.2.x behaviour).
//	                       Extension hint, if present, is ignored.
//	StrategyExtension    — pick by filename extension allowlist only.
//	                       MIME, if present, is ignored. Useful when the
//	                       operator does NOT trust client-declared MIME.
//	StrategyMimeThenExt  — try MIME first; if no non-default engine
//	                       claims the request, fall back to extension
//	                       dispatch; finally default. This is the
//	                       recommended default — preserves v0.2.x
//	                       semantics when no engine declares
//	                       `extensions`, and gracefully degrades to
//	                       extension routing for legacy / generic
//	                       Content-Type clients.
//	StrategyExtThenMime  — try extension first; if filename has no
//	                       recognised extension OR no engine matches,
//	                       fall back to MIME dispatch; finally default.
const (
	StrategyMime        RoutingStrategy = "mime"
	StrategyExtension   RoutingStrategy = "extension"
	StrategyMimeThenExt RoutingStrategy = "mime_then_extension"
	StrategyExtThenMime RoutingStrategy = "extension_then_mime"
)

// PickHint bundles every routing signal in a struct so future signals
// (filename, declared/sniffed MIME, content-length class, …) slot in
// without churning the Registry method signature. The zero value
// represents "no information" — Registry treats it as "fall through
// to default engine".
type PickHint struct {
	// MIME is the resolved (post-sniff) media type, lowercased,
	// parameters stripped. Empty means no MIME signal.
	MIME string
	// Filename is the client-supplied multipart filename (already
	// passed through SafeMultipartFilename so it's ASCII-clean).
	// Used only for its extension via filepath.Ext. Empty means no
	// filename signal.
	Filename string
}

// PickSource names the matcher that won the dispatch decision. The
// handlers surface this on the access log (`pick_source` field) so
// operators can answer "why did the proxy route this request to
// engine X?" from one log line.
type PickSource string

const (
	// PickSourceMIME — engine matched on the MIME phase.
	PickSourceMIME PickSource = "mime"
	// PickSourceExtension — engine matched on the extension phase.
	PickSourceExtension PickSource = "extension"
	// PickSourceDefault — no non-default engine claimed; the
	// configured default engine handled the request as the final
	// fallback (the existing v0.2.x catch-all contract).
	PickSourceDefault PickSource = "default"
)

// ResolveStrategy normalises an operator-supplied strategy value.
// Empty resolves to StrategyMimeThenExt — that's the operator-facing
// default AND the safe choice when an old config (predating this
// feature) loads without the field.
func ResolveStrategy(s RoutingStrategy) RoutingStrategy {
	switch s {
	case StrategyMime, StrategyExtension, StrategyMimeThenExt, StrategyExtThenMime:
		return s
	default:
		return StrategyMimeThenExt
	}
}
