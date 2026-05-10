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
type Engine interface {
	Name() Name
	Capabilities() EngineCapabilities
	Convert(ctx context.Context, req *ConvertRequest) (*ConvertResponse, error)
	Health(ctx context.Context) error
}

// Registry is a read-only lookup of enabled engines. Populated once at
// startup and immutable afterwards (no locking needed on the hot path).
type Registry interface {
	Get(name Name) (Engine, error)
	Default() Engine
	// Pick returns the engine that should handle a request whose first
	// file carries the given MIME type and arrived via facade. Each
	// non-default enabled engine is checked in registration order;
	// the first one whose Capabilities include the facade AND whose
	// configured patterns match the MIME wins. The default engine is
	// returned only when it itself accepts the facade — otherwise
	// ErrNoEngineForFacade is returned via PickWithError; the
	// convenience Pick returns the default unconditionally and lets
	// the caller's request fail naturally.
	Pick(facade Facade, mime string) Engine
	// PickWithError is the strict form. It returns ErrNoEngineForFacade
	// if no enabled engine answers the facade.
	PickWithError(facade Facade, mime string) (Engine, error)
	Names() []Name
}

// Errors.
var (
	ErrUnknownEngine     = errors.New("engine: unknown engine name")
	ErrEngineDisabled    = errors.New("engine: engine is disabled")
	ErrNoEngineForFacade = errors.New("engine: no enabled engine accepts the requested facade")
)

// RegistryEntry pairs an engine with the MIME patterns it claims.
// Patterns may be exact ("application/pdf") or top-level wildcards
// ("image/*"). Matching is case-insensitive.
type RegistryEntry struct {
	Engine    Engine
	MimeTypes []string
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
	pickOrder []namedPattern
}

// namedPattern bundles an engine with its compiled MIME rules so Pick
// avoids a second map lookup per candidate.
type namedPattern struct {
	engine   Engine
	patterns []mimePattern
}

// NewRegistry returns a Registry whose contents are fixed at construction
// time. defaultEngine MUST be present in entries.
func NewRegistry(entries map[Name]RegistryEntry, defaultEngine Name) (Registry, error) {
	if len(entries) == 0 {
		return nil, errors.New("engine: at least one engine must be registered")
	}
	defEntry, ok := entries[defaultEngine]
	if !ok {
		return nil, ErrUnknownEngine
	}

	engines := make(map[Name]Engine, len(entries))
	mimeMap := make(map[Name][]mimePattern, len(entries))
	nonDefault := make([]Name, 0, len(entries))

	for n, e := range entries {
		engines[n] = e.Engine
		mimeMap[n] = compilePatterns(e.MimeTypes)
		if n == defaultEngine {
			continue
		}
		nonDefault = append(nonDefault, n)
	}
	stringSort(nonDefault)
	order := append(append([]Name{}, nonDefault...), defaultEngine)

	pickOrder := make([]namedPattern, 0, len(nonDefault))
	for _, n := range nonDefault {
		pickOrder = append(pickOrder, namedPattern{
			engine:   engines[n],
			patterns: mimeMap[n],
		})
	}

	return &staticRegistry{
		engines:   engines,
		def:       defEntry.Engine,
		defName:   defaultEngine,
		order:     order,
		pickOrder: pickOrder,
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

func (s *staticRegistry) Pick(facade Facade, mime string) Engine {
	if eng, err := s.PickWithError(facade, mime); err == nil {
		return eng
	}
	// Permissive form: fall back to default even if it does not declare
	// the facade. Calls that need strict 4xx behaviour use
	// PickWithError. This keeps test fixtures simple.
	return s.def
}

func (s *staticRegistry) PickWithError(facade Facade, mime string) (Engine, error) {
	mime = canonicalMIME(mime)
	// Phase 1: scan non-default engines that answer the facade.
	for i := range s.pickOrder {
		np := &s.pickOrder[i]
		if !np.engine.Capabilities().AcceptsFacade(facade) {
			continue
		}
		if mime == "" {
			// no MIME hint → skip non-default candidates; let default
			// handle (matches the v0.1.x pre-Capabilities semantics).
			continue
		}
		for j := range np.patterns {
			if np.patterns[j].matches(mime) {
				return np.engine, nil
			}
		}
	}
	// Phase 2: default engine, only if it accepts this facade.
	if s.def.Capabilities().AcceptsFacade(facade) {
		return s.def, nil
	}
	return nil, ErrNoEngineForFacade
}

func (s *staticRegistry) Names() []Name {
	out := make([]Name, len(s.order))
	copy(out, s.order)
	return out
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
