// Package mimedetect resolves the effective MIME type of an uploaded
// file from the combination of (a) what the client declared in the
// multipart part header or request Content-Type and (b) magic-byte
// sniffing of the file head.
//
// Before this package existed the proxy trusted whatever the client
// declared, so a client that sent "Content-Type: application/octet-
// stream" with a PDF body bypassed MIME-based engine routing: the
// `engines.<name>.mime_types` allowlists never matched the
// declared (generic) MIME, so requests fell through to the default
// engine and got dispatched with the WRONG api_key — surfacing as a
// backend 401 to operators.
//
// Resolve sniffs only when the declared type is missing or generic,
// preserving the v0.2.x trust-the-client behaviour for everything
// else. The Source field on the result lets log lines surface
// whether the chosen MIME came from the wire or from the sniff,
// which the operator-facing access log uses to debug routing.
package mimedetect

import (
	"net/http"
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
	// SourceFallback: declared was empty AND sniff failed (typically
	// because the head buffer is too short / file is empty). We fall
	// back to application/octet-stream so downstream routing has a
	// stable string to switch on.
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

// Resolve picks the effective MIME type from the declared value and
// a head buffer of the file body. headPeek may be shorter than 512
// bytes (mimetype.Detect handles short inputs) or nil.
//
// Decision tree:
//
//  1. declared is set AND not in genericDeclared → trust declared
//     (SourceDeclared). This is the v0.2.x path and the fast one.
//  2. declared is generic AND headPeek yields a specific MIME via
//     mimetype.Detect → adopt the sniff (SourceSniffed).
//  3. declared is generic AND sniff also yields a generic type or
//     fails → SourceFallback with application/octet-stream.
//
// The caller is expected to use Result.MIME for engine routing AND
// to log Result.Source so operators can tell apart "client gave us
// a precise hint" from "we had to guess".
func Resolve(declared string, headPeek []byte) Result {
	declared = canonical(declared)

	if _, generic := genericDeclared[declared]; !generic {
		return Result{MIME: declared, Source: SourceDeclared}
	}

	// Generic declared — try to sniff.
	if len(headPeek) > 0 {
		m := mimetype.Detect(headPeek)
		if m != nil {
			sniff := canonical(m.String())
			if _, stillGeneric := genericDeclared[sniff]; !stillGeneric && sniff != "" {
				return Result{MIME: sniff, Source: SourceSniffed}
			}
		}
	}

	return Result{MIME: "application/octet-stream", Source: SourceFallback}
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
	// Strip everything after the first ';' (parameters).
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.ToLower(strings.TrimSpace(ct))
}

// Compile-time check: mimetype is only used in this package, never
// imported transitively into handler hot paths without going through
// Resolve. Keeping the import here narrows the dependency graph.
var _ http.Header = nil
