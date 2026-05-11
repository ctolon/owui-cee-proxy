// Package mimedetect resolves the effective MIME type of an uploaded
// file from the combination of (a) what the client declared in the
// multipart part header or request Content-Type, (b) magic-byte
// sniffing of the file head, and (c) the filename extension as a
// final disambiguator for shared-magic formats (Compound File Binary).
//
// Before this package existed the proxy trusted whatever the client
// declared, so a client that sent "Content-Type: application/octet-
// stream" with a PDF body bypassed MIME-based engine routing: the
// `engines.<name>.mime_types` allowlists never matched the
// declared (generic) MIME, so requests fell through to the default
// engine and got dispatched with the WRONG api_key — surfacing as a
// backend 401 to operators.
//
// v0.5.0 added the filename-aware extension fallback. Several legacy
// Office formats (.msg, .doc, .xls, .ppt) share the same Compound
// File Binary magic header (`D0CF11E0`); mimetype.Detect collapses
// the whole family to `application/vnd.ms-powerpoint` because
// PowerPoint was the first family member it learned. Without a
// filename hint the proxy used to dispatch Outlook .msg uploads to
// PowerPoint-claiming engines, which 400'd at the backend. The
// Resolver merges a built-in extension map with an operator-supplied
// override map (Mimedetect.ExtensionOverrides) so deployments can
// extend the disambiguation set without code changes.
package mimedetect

import (
	"maps"
	"strings"

	"github.com/gabriel-vasile/mimetype"
)

// Source identifies how the final MIME type was chosen. Exported so
// AccessLog and the upstream debug log can surface it as a field.
type Source string

const (
	// SourceDeclared: the client sent a specific Content-Type and we
	// trusted it (the common, fast path).
	SourceDeclared Source = "declared"
	// SourceSniffed: the declared type was missing or generic
	// (`application/octet-stream`, `binary/octet-stream`, or "")
	// and mimetype.Detect produced a more specific answer that we
	// adopted.
	SourceSniffed Source = "sniffed"
	// SourceSniffedThenExtension: declared was generic and sniff
	// produced a CFB-ambiguous MIME (e.g.,
	// `application/vnd.ms-powerpoint` from a `.msg` body). The
	// filename's extension narrowed it down to a more specific MIME
	// via the extension map. The sniff "claimed" the file as CFB,
	// the extension "named" which CFB family it belongs to.
	SourceSniffedThenExtension Source = "sniffed_then_extension"
	// SourceExtension: declared was generic AND sniff failed or also
	// returned a generic MIME. The filename's extension was the sole
	// signal that produced a routable MIME via the extension map.
	SourceExtension Source = "extension"
	// SourceFallback: nothing matched — declared was empty/generic,
	// sniff failed/generic, filename had no recognised extension. We
	// fall back to application/octet-stream so downstream routing has
	// a stable string to switch on.
	SourceFallback Source = "fallback"
)

// Result is what Resolve returns. MIME is always a canonical lower-
// cased media type without parameters (parameters such as
// `; charset=utf-8` are stripped — engine selection runs on the
// bare type, never the parameters).
type Result struct {
	MIME   string
	Source Source
}

// Generic Content-Type values that ALSO trigger a sniff. These are
// the "I don't know" tokens HTTP clients use; they carry no routing
// signal so we override them with sniffed MIME whenever sniffing
// produces something more specific.
//
// Note: text/plain is intentionally NOT in this set. text/plain is
// a real, routable type for many extractors (Tika, Kreuzberg) and a
// client that explicitly sends text/plain has the strongest claim
// on the routing decision.
var genericDeclared = map[string]struct{}{
	"":                         {},
	"application/octet-stream": {},
	"binary/octet-stream":      {},
}

// cfbAmbiguousSniffs are MIME values mimetype.Detect can return for
// any Compound File Binary container. Every Office 97-2003 format
// (Word .doc, Excel .xls, PowerPoint .ppt) plus Outlook .msg shares
// the same `D0CF11E0` header — the library can't tell them apart
// from magic bytes alone, so a sniff in this set is a STRONG hint
// that the file is CFB-family but a WEAK hint about which family
// member. Combined with the filename extension, we can be precise.
var cfbAmbiguousSniffs = map[string]struct{}{
	"application/vnd.ms-powerpoint": {},
	"application/vnd.ms-office":     {},
	"application/x-cfb":             {},
	"application/x-ole-storage":     {},
	"application/msword":            {},
	"application/vnd.ms-excel":      {},
}

// builtinExtensionMap is the immutable, conservative default mapping
// from filename extension to canonical MIME used by the disambiguation
// step. The 10 entries here are the formats the proxy has seen
// silently misroute in the wild plus their modern (Office Open XML)
// cousins for symmetry; operators may extend or replace any entry
// via Mimedetect.ExtensionOverrides in the YAML config.
//
// Keys MUST be leading-dot, lowercase, single-segment.
var builtinExtensionMap = map[string]string{
	// Legacy Office CFB family.
	".msg": "application/vnd.ms-outlook",
	".doc": "application/msword",
	".xls": "application/vnd.ms-excel",
	".ppt": "application/vnd.ms-powerpoint",
	// Office Open XML — non-CFB but listed for completeness so
	// operators get extension routing for them too without extra YAML.
	".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	// Email + tabular data — common upload types where the sniff is
	// frequently text/plain (or fails outright) and the extension is
	// the only reliable signal.
	".eml": "message/rfc822",
	".csv": "text/csv",
	".tsv": "text/tab-separated-values",
}

// Resolver applies the MIME resolution policy with an effective
// extension→MIME table derived from the built-in defaults plus any
// operator-supplied overrides. The zero value is unusable — call
// NewResolver. Construction is cheap (one map merge); the result is
// safe for concurrent use because the map is read-only after Build.
type Resolver struct {
	extMap map[string]string
}

// NewResolver returns a Resolver whose effective extension map is the
// built-in table merged with overrides. Override entries replace
// built-in entries on key collision (operator intent wins). Keys and
// values are lowercased for symmetric matching with the rest of the
// pipeline; the config validator (validateMimedetect) already
// enforces lowercase + leading-dot at Load time, this defensive
// normalisation is belt-and-braces against programmatic callers.
//
// A nil overrides argument is the bootstrap case — the Resolver
// behaves identically to the package-level Resolve shim.
func NewResolver(overrides map[string]string) *Resolver {
	eff := make(map[string]string, len(builtinExtensionMap)+len(overrides))
	maps.Copy(eff, builtinExtensionMap)
	for k, v := range overrides {
		eff[strings.ToLower(strings.TrimSpace(k))] = strings.ToLower(strings.TrimSpace(v))
	}
	return &Resolver{extMap: eff}
}

// defaultResolver backs the package-level Resolve shim. It has only
// the built-in extension map — no operator overrides. Constructed
// once at init so the package-level entry point remains zero-cost
// for callers that don't need overrides.
var defaultResolver = NewResolver(nil)

// Default returns the package-level resolver that has only the
// built-in extension map (no operator overrides). Use this as a safe
// fallback in tests or call sites where injecting a fully-configured
// Resolver would be overkill.
func Default() *Resolver { return defaultResolver }

// Resolve is the package-level shim that uses the built-in extension
// map only. All in-process callers should prefer constructing a
// Resolver in the composition root and injecting it; this shim
// exists for tests and for transitional call sites.
func Resolve(declared string, headPeek []byte, filename string) Result {
	return defaultResolver.Resolve(declared, headPeek, filename)
}

// Resolve picks the effective MIME type for an uploaded file.
//
// Decision tree (in order, first match wins):
//
//  1. declared is set AND not in genericDeclared → trust declared
//     (SourceDeclared). The fast, v0.2.x-compatible path.
//  2. declared is generic AND sniff produces a non-generic,
//     non-CFB-ambiguous MIME → adopt the sniff (SourceSniffed).
//  3. declared is generic AND sniff produces a CFB-ambiguous MIME
//     AND filename extension maps to a known MIME → use the mapped
//     MIME (SourceSniffedThenExtension). This is the .msg fix.
//  4. declared is generic AND sniff fails OR is generic AND filename
//     extension maps to a known MIME → use the mapped MIME
//     (SourceExtension). Covers .csv / .tsv where the sniff returns
//     text/plain (generic enough that we'd rather trust the
//     extension).
//  5. Nothing matched → application/octet-stream (SourceFallback).
//
// Empty filename simply skips the extension lookup; the rest of the
// tree behaves identically. headPeek may be shorter than 512 bytes
// (mimetype.Detect handles short inputs) or nil.
func (r *Resolver) Resolve(declared string, headPeek []byte, filename string) Result {
	declared = canonical(declared)

	if _, generic := genericDeclared[declared]; !generic {
		return Result{MIME: declared, Source: SourceDeclared}
	}

	sniff := ""
	if len(headPeek) > 0 {
		if m := mimetype.Detect(headPeek); m != nil {
			sniff = canonical(m.String())
		}
	}
	_, sniffGeneric := genericDeclared[sniff]
	_, sniffAmbig := cfbAmbiguousSniffs[sniff]

	// Phase 2 — sniff is concrete and not CFB-ambiguous: adopt it.
	if sniff != "" && !sniffGeneric && !sniffAmbig {
		return Result{MIME: sniff, Source: SourceSniffed}
	}

	extMIME := r.lookupByFilename(filename)

	// Phase 3 — sniff is CFB-ambiguous, filename disambiguates it.
	if sniffAmbig && extMIME != "" {
		return Result{MIME: extMIME, Source: SourceSniffedThenExtension}
	}

	// Phase 4 — sniff failed / generic, filename has a usable
	// extension. (No sniff signal at all also lands here.)
	if extMIME != "" && (sniff == "" || sniffGeneric) {
		return Result{MIME: extMIME, Source: SourceExtension}
	}

	// Phase 3' — CFB-ambig sniff and NO filename hit. The sniff is
	// still better than octet-stream; we keep it. Operator routing
	// will either treat the ambiguous MIME as routable (engines
	// declare it in their mime_types) or fall back to the default
	// engine.
	if sniffAmbig {
		return Result{MIME: sniff, Source: SourceSniffed}
	}

	// Phase 5 — nothing worked. Stable fallback.
	return Result{MIME: "application/octet-stream", Source: SourceFallback}
}

// lookupByFilename returns the mapped MIME for the filename's
// extension, or "" when the filename has no usable extension or no
// entry is registered for it. Filename is matched on its last
// segment's last-dot suffix only — no glob, no compound extensions
// (".tar.gz" → ".gz"), matching path/filepath.Ext semantics.
func (r *Resolver) lookupByFilename(filename string) string {
	ext := extOf(filename)
	if ext == "" {
		return ""
	}
	return r.extMap[ext]
}

// extOf is a tiny zero-allocation extractor that returns the
// lowercased terminal extension (including leading dot) of filename,
// or "" when there is no extension. Mirrors path/filepath.Ext but
// strips path separators so it tolerates filename strings that still
// carry directory segments. Kept local so the package doesn't drag
// path/filepath into a hot path that runs once per uploaded part.
func extOf(name string) string {
	for i := len(name) - 1; i >= 0; i-- {
		switch name[i] {
		case '/', '\\':
			return ""
		case '.':
			return strings.ToLower(name[i:])
		}
	}
	return ""
}

// canonical strips Content-Type parameters and lowercases the bare
// media type. Net/http's mime.ParseMediaType would be more correct
// but adds an error path we don't need — Content-Type values that
// fail to parse via ParseMediaType degrade to "" here and then
// resolve to the sniffed/fallback path. Same semantics, less code.
func canonical(ct string) string {
	ct = strings.TrimSpace(ct)
	if ct == "" {
		return ""
	}
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.ToLower(strings.TrimSpace(ct))
}
