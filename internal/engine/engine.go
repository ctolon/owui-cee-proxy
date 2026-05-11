// Package engine defines the abstraction every Content Extraction Engine
// adapter must implement. The transport layer depends only on this package
// (Dependency Inversion Principle); concrete adapters live under
// internal/engine/compat/<compat_type>.
//
// Adding a new engine requires implementing Engine, registering the
// constructor in the composition root for one of the existing
// compat_type values (or adding a new one), and adding the
// corresponding entry to the YAML schema. No transport or handler code
// needs changes.
package engine

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
)

// Name aliases the engine identifier used in config and logs. Engine
// names are user-supplied (the YAML key under `engines:`); they MUST
// match `^[a-z0-9-]{1,32}$` so they're safe to use as URL path
// segments for the passthrough mounts.
type Name string

// Facade identifies which client-facing protocol the inbound request
// arrived through. It travels in the ConvertRequest so engine adapters
// can answer the right shape (chiefly relevant to compat_type:
// docling-external, which has a child adapter per facade).
type Facade string

const (
	FacadeDocling  Facade = "docling"
	FacadeExternal Facade = "external"
)

// FileBlob is a single file part of a multipart upload, streamed.
type FileBlob struct {
	Filename    string
	ContentType string
	Size        int64 // -1 if unknown
	Body        io.Reader
}

// HTTPSource describes a remote URL the engine should fetch (used by the
// Docling-compatible /v1/convert/source endpoint).
type HTTPSource struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
}

// ConvertRequest is the engine-agnostic request shape produced by the
// transport layer. Adapters translate it into their backend's native
// format and translate the response back to the shape that matches the
// inbound facade.
type ConvertRequest struct {
	Files     []FileBlob
	Sources   []HTTPSource
	Options   map[string]string
	Headers   http.Header
	RequestID string
	// Facade records which inbound facade produced this request. The
	// docling-external composite adapter routes by this; other adapters
	// may use it to choose the response shape.
	Facade Facade
}

// ConvertResponse carries the engine response back to the handler.
// Body is streamed; the caller MUST close it.
type ConvertResponse struct {
	StatusCode int
	Headers    http.Header
	Body       io.ReadCloser
}

// EngineCapabilities advertises an engine's protocol surface. The
// transport layer consults this:
//   - to gate /v1/convert/source on engines whose backend does not
//     support remote URLs (replaces the v0.1.x hard-coded "Docling
//     only" check).
//   - to filter Pick(facade, mime) candidates by facade.
type EngineCapabilities struct {
	// Facades lists every facade this engine answers via.
	Facades []Facade
	// HTTPSources is true when the engine accepts /v1/convert/source
	// (i.e., remote URL fetch). Currently only Docling Serve does.
	HTTPSources bool
}

// AcceptsFacade reports whether this engine answers via f.
func (c EngineCapabilities) AcceptsFacade(f Facade) bool {
	for _, x := range c.Facades {
		if x == f {
			return true
		}
	}
	return false
}

// Engine is implemented by each adapter.
//
// URL returns the configured base URL of the backend the adapter is
// pointed at (the engines.<name>.url from YAML). This is for operator
// observability ONLY — the proxy's request lifecycle uses it as a log
// field and span attribute so logs/traces can attribute a particular
// request to a particular backend. Adapters MUST NOT use URL to make
// routing decisions; routing is decided by Capabilities + Registry.
type Engine interface {
	Name() Name
	URL() string
	Capabilities() EngineCapabilities
	Convert(ctx context.Context, req *ConvertRequest) (*ConvertResponse, error)
	Health(ctx context.Context) error
}

// Registry is a read-only lookup of enabled engines. Populated once at
// startup and immutable afterwards (no locking needed on the hot path).
type Registry interface {
	Get(name Name) (Engine, error)
	Default() Engine
	// Pick is the legacy MIME-only shim retained for callers that
	// haven't migrated to PickRoute. Internally delegates to
	// PickRoute with an empty filename and the configured strategy.
	// Falls back to default on no-match (permissive form).
	Pick(facade Facade, mime string) Engine
	// PickWithError is the legacy strict shim — returns
	// ErrNoEngineForFacade when no engine accepts the facade.
	PickWithError(facade Facade, mime string) (Engine, error)
	// PickRoute is the modern dispatcher. It consults BOTH the MIME
	// allowlist and the filename-extension allowlist according to
	// the registry's configured RoutingStrategy. Returns the engine
	// or ErrNoEngineForFacade.
	PickRoute(facade Facade, hint PickHint) (Engine, error)
	// PickRouteVerbose returns the engine AND the matcher (MIME |
	// extension | default) that won the dispatch. Surface this in
	// observability log lines (`pick_source` field) so operators
	// can audit routing decisions from one record.
	PickRouteVerbose(facade Facade, hint PickHint) (Engine, PickSource, error)
	// Strategy returns the registry's configured routing strategy.
	// Handlers stamp it into the request-scoped log context so the
	// access log can carry `routing_strategy` alongside `pick_source`.
	Strategy() RoutingStrategy
	Names() []Name
}

// Errors.
var (
	ErrUnknownEngine     = errors.New("engine: unknown engine name")
	ErrEngineDisabled    = errors.New("engine: engine is disabled")
	ErrNoEngineForFacade = errors.New("engine: no enabled engine accepts the requested facade")
)

// RegistryEntry pairs an engine with the MIME + extension allowlists
// that govern when it gets picked. MimeTypes may be exact
// ("application/pdf") or top-level wildcards ("image/*"). Extensions
// are exact, leading-dot, lowercased (operator input is normalised
// at compile time). Empty list = engine abstains from that phase of
// dispatch — symmetrical to v0.2.x's empty-MimeTypes contract.
type RegistryEntry struct {
	Engine     Engine
	MimeTypes  []string
	Extensions []string
}

// staticRegistry is an immutable Registry. Use NewRegistry to construct.
type staticRegistry struct {
	engines map[Name]Engine
	def     Engine
	defName Name
	// order lists every engine alphabetically (default last); used by
	// Names() so the public registration order is stable.
	order []Name
	// pickOrder pre-computes the non-default engines in the order
	// Pick() must scan them — alphabetical, default omitted.
	pickOrder []namedRoute
	// strategy is resolved at construction (empty → mime_then_extension).
	strategy RoutingStrategy
}

// namedRoute bundles an engine with its compiled MIME + extension
// rules so PickRoute avoids a second map lookup per candidate.
type namedRoute struct {
	engine   Engine
	mimePats []mimePattern
	extPats  []extPattern
}

// NewRegistry returns a Registry whose contents are fixed at construction
// time. defaultEngine MUST be present in entries. strategy=="" resolves
// to StrategyMimeThenExt (the operator-friendly default that preserves
// v0.2.x behaviour when no engine declares an `extensions` allowlist).
func NewRegistry(entries map[Name]RegistryEntry, defaultEngine Name, strategy RoutingStrategy) (Registry, error) {
	if len(entries) == 0 {
		return nil, errors.New("engine: at least one engine must be registered")
	}
	defEntry, ok := entries[defaultEngine]
	if !ok {
		return nil, ErrUnknownEngine
	}

	engines := make(map[Name]Engine, len(entries))
	mimeMap := make(map[Name][]mimePattern, len(entries))
	extMap := make(map[Name][]extPattern, len(entries))
	nonDefault := make([]Name, 0, len(entries))

	for n, e := range entries {
		engines[n] = e.Engine
		mimeMap[n] = compilePatterns(e.MimeTypes)
		extMap[n] = compileExtensions(e.Extensions)
		if n == defaultEngine {
			continue
		}
		nonDefault = append(nonDefault, n)
	}
	stringSort(nonDefault)
	order := append(append([]Name{}, nonDefault...), defaultEngine)

	pickOrder := make([]namedRoute, 0, len(nonDefault))
	for _, n := range nonDefault {
		pickOrder = append(pickOrder, namedRoute{
			engine:   engines[n],
			mimePats: mimeMap[n],
			extPats:  extMap[n],
		})
	}

	return &staticRegistry{
		engines:   engines,
		def:       defEntry.Engine,
		defName:   defaultEngine,
		order:     order,
		pickOrder: pickOrder,
		strategy:  ResolveStrategy(strategy),
	}, nil
}

func (s *staticRegistry) Get(name Name) (Engine, error) {
	e, ok := s.engines[name]
	if !ok {
		return nil, ErrUnknownEngine
	}
	return e, nil
}

func (s *staticRegistry) Default() Engine { return s.def }

func (s *staticRegistry) Strategy() RoutingStrategy { return s.strategy }

// Pick (legacy) — MIME-only shim. Delegates to PickRoute with empty
// filename; permissive form falls back to default on no-match.
func (s *staticRegistry) Pick(facade Facade, mime string) Engine {
	if eng, err := s.PickRoute(facade, PickHint{MIME: mime}); err == nil {
		return eng
	}
	return s.def
}

// PickWithError (legacy) — strict MIME-only shim.
func (s *staticRegistry) PickWithError(facade Facade, mime string) (Engine, error) {
	return s.PickRoute(facade, PickHint{MIME: mime})
}

// PickRoute is the modern dispatcher. Delegates to PickRouteVerbose
// and discards the PickSource for callers that don't need it.
func (s *staticRegistry) PickRoute(facade Facade, hint PickHint) (Engine, error) {
	eng, _, err := s.PickRouteVerbose(facade, hint)
	return eng, err
}

// PickRouteVerbose drives the 4-strategy dispatch. The decision tree:
//
//  1. Build the phase sequence from s.strategy:
//     mime                 → [mime]
//     extension            → [ext]
//     mime_then_extension  → [mime, ext]   // default
//     extension_then_mime  → [ext, mime]
//  2. Each phase walks pickOrder (non-default engines, alphabetical)
//     and returns the first match. Phases short-circuit — once a
//     matcher claims the request, no later phase runs.
//  3. If no phase matched, fall back to the default engine when it
//     accepts the facade; otherwise ErrNoEngineForFacade.
//
// Empty hint (no MIME and no filename) never claims at any phase →
// the request goes straight to the default. This preserves the
// "source mode falls back to default" semantics convert.go relies on.
func (s *staticRegistry) PickRouteVerbose(facade Facade, hint PickHint) (Engine, PickSource, error) {
	mime := canonicalMIME(hint.MIME)
	ext := strings.ToLower(filepathExt(hint.Filename))

	phases := strategyPhases(s.strategy)
	for _, ph := range phases {
		switch ph {
		case phaseMime:
			if mime == "" {
				continue
			}
			for i := range s.pickOrder {
				np := &s.pickOrder[i]
				if !np.engine.Capabilities().AcceptsFacade(facade) {
					continue
				}
				for j := range np.mimePats {
					if np.mimePats[j].matches(mime) {
						return np.engine, PickSourceMIME, nil
					}
				}
			}
		case phaseExt:
			if ext == "" {
				continue
			}
			for i := range s.pickOrder {
				np := &s.pickOrder[i]
				if !np.engine.Capabilities().AcceptsFacade(facade) {
					continue
				}
				for j := range np.extPats {
					if np.extPats[j].matches(ext) {
						return np.engine, PickSourceExtension, nil
					}
				}
			}
		}
	}
	// Final fallback: default engine, only if it accepts the facade.
	if s.def.Capabilities().AcceptsFacade(facade) {
		return s.def, PickSourceDefault, nil
	}
	return nil, PickSourceDefault, ErrNoEngineForFacade
}

// dispatchPhase enumerates the phases PickRouteVerbose runs in order.
// Kept as a typed int constant set (rather than a closure list) so
// the inner switch stays inlineable on the hot path.
type dispatchPhase int

const (
	phaseMime dispatchPhase = iota + 1
	phaseExt
)

func strategyPhases(s RoutingStrategy) []dispatchPhase {
	switch s {
	case StrategyMime:
		return []dispatchPhase{phaseMime}
	case StrategyExtension:
		return []dispatchPhase{phaseExt}
	case StrategyExtThenMime:
		return []dispatchPhase{phaseExt, phaseMime}
	case StrategyMimeThenExt:
		fallthrough
	default:
		return []dispatchPhase{phaseMime, phaseExt}
	}
}

func (s *staticRegistry) Names() []Name {
	out := make([]Name, len(s.order))
	copy(out, s.order)
	return out
}

// filepathExt is a tiny local wrapper around path/filepath.Ext.
// Keeping it in this file avoids a path/filepath import sprawl
// across the package (only Pick reads filename extensions). Returns
// "" for empty input or when the basename has no dot.
func filepathExt(name string) string {
	for i := len(name) - 1; i >= 0; i-- {
		switch name[i] {
		case '/', '\\':
			return ""
		case '.':
			return name[i:]
		}
	}
	return ""
}

// canonicalMIME strips parameters (e.g., "; charset=utf-8") and
// lowercases the type so matching is stable.
func canonicalMIME(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, ';'); i >= 0 {
		s = s[:i]
	}
	return strings.ToLower(strings.TrimSpace(s))
}

// stringSort is a tiny stable sort to avoid pulling sort.Slice into the
// hot path (registration time only — fine, but small alloc savings).
func stringSort(in []Name) []Name {
	for i := 1; i < len(in); i++ {
		for j := i; j > 0 && in[j-1] > in[j]; j-- {
			in[j-1], in[j] = in[j], in[j-1]
		}
	}
	return in
}
