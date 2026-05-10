// Package engine defines the abstraction every Content Extraction Engine
// adapter must implement. The transport layer depends only on this package
// (Dependency Inversion Principle); concrete adapters live in subpackages
// under internal/engine/<name>.
//
// Adding a new engine requires implementing Engine, registering the
// constructor in the composition root, and adding the corresponding
// section to the YAML schema. No transport or handler code needs changes.
package engine

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
)

// Name aliases the engine identifier used in config and logs.
type Name string

const (
	Docling   Name = "docling"
	Tika      Name = "tika"
	Kreuzberg Name = "kreuzberg"
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
// format and translate the response back to the Docling-compatible
// shape returned in ConvertResponse.
type ConvertRequest struct {
	Files     []FileBlob
	Sources   []HTTPSource
	Options   map[string]string
	Headers   http.Header
	RequestID string
}

// ConvertResponse carries the engine response back to the handler.
// Body is streamed; the caller MUST close it.
type ConvertResponse struct {
	StatusCode int
	Headers    http.Header
	Body       io.ReadCloser
}

// Engine is implemented by each adapter.
type Engine interface {
	Name() Name
	Convert(ctx context.Context, req *ConvertRequest) (*ConvertResponse, error)
	Health(ctx context.Context) error
}

// Registry is a read-only lookup of enabled engines. Populated once at
// startup and immutable afterwards (no locking needed on the hot path).
type Registry interface {
	Get(name Name) (Engine, error)
	Default() Engine
	// Pick returns the engine that should handle a request whose first
	// file carries the given MIME type. The lookup tries each enabled
	// non-default engine in registration order; the first one whose
	// configured patterns match wins. Default is returned when no
	// non-default engine matches (or when mime is empty).
	Pick(mime string) Engine
	Names() []Name
}

// RegistryEntry pairs an engine with the MIME patterns it claims.
// Patterns may be exact ("application/pdf") or top-level wildcards
// ("image/*"). Matching is case-insensitive.
type RegistryEntry struct {
	Engine    Engine
	MimeTypes []string
}

// ErrUnknownEngine is returned by Registry.Get when no adapter is
// registered for the given name.
var ErrUnknownEngine = errors.New("engine: unknown engine name")

// ErrEngineDisabled is returned when the engine exists but is disabled
// in configuration.
var ErrEngineDisabled = errors.New("engine: engine is disabled")

// staticRegistry is an immutable Registry. Use NewRegistry to construct.
type staticRegistry struct {
	engines map[Name]Engine
	def     Engine
	defName Name
	// order lists every engine alphabetically; used by Names() so the
	// public registration order is stable (default last).
	order []Name
	// pickOrder pre-computes the non-default engines in the order Pick()
	// must scan them — alphabetical, default omitted. Iterating this
	// slice lets Pick() skip the per-iteration "is this the default?"
	// branch, which N4 in docs/REVIEW.md flagged as dead work on the
	// hot path.
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

	// Deterministic registration order: alphabetical, with default last
	// so non-default engines are checked first by Pick.
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

func (s *staticRegistry) Pick(mime string) Engine {
	mime = canonicalMIME(mime)
	if mime == "" {
		return s.def
	}
	for i := range s.pickOrder {
		np := &s.pickOrder[i]
		for j := range np.patterns {
			if np.patterns[j].matches(mime) {
				return np.engine
			}
		}
	}
	return s.def
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
