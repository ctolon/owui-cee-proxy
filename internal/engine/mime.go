package engine

import "strings"

// mimePattern is a compiled MIME match rule. Two forms are supported:
//   - exact: "application/pdf"
//   - top-level wildcard: "image/*"
//
// Matching is case-insensitive and ignores parameters
// (e.g., "; charset=utf-8") on the input — see canonicalMIME.
type mimePattern struct {
	full   string // exact match (lowercased), empty if wildcard
	prefix string // for wildcards: "image/" matches "image/*"
}

func compilePatterns(in []string) []mimePattern {
	out := make([]mimePattern, 0, len(in))
	for _, s := range in {
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "" {
			continue
		}
		if strings.HasSuffix(s, "/*") {
			out = append(out, mimePattern{prefix: strings.TrimSuffix(s, "*")})
			continue
		}
		if s == "*/*" {
			out = append(out, mimePattern{prefix: ""}) // matches anything; use sparingly
			continue
		}
		out = append(out, mimePattern{full: s})
	}
	return out
}

func (p mimePattern) matches(mime string) bool {
	if p.full != "" {
		return p.full == mime
	}
	// wildcard form
	return strings.HasPrefix(mime, p.prefix)
}

// extPattern is the filename-extension counterpart of mimePattern.
// One form only — exact match against a leading-dot, lowercased
// extension (e.g., ".msg"). No glob support is intentional: filepath.Ext
// only returns the last suffix, so glob semantics over multi-segment
// extensions would require a custom matcher; explicit lists keep the
// dispatch deterministic and audit-friendly.
//
// Operator input is normalised at compile time (compileExtensions):
// "PDF", "pdf", and ".PDF" all become ".pdf" patterns. Empty / blank
// inputs are dropped silently.
type extPattern struct {
	ext string // ".pdf", ".msg", ... — lowercased, leading dot, never empty
}

func compileExtensions(in []string) []extPattern {
	out := make([]extPattern, 0, len(in))
	for _, s := range in {
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "" {
			continue
		}
		if !strings.HasPrefix(s, ".") {
			s = "." + s
		}
		out = append(out, extPattern{ext: s})
	}
	return out
}

// matches compares ext (must already be lowercased, with leading dot,
// e.g., from `strings.ToLower(filepath.Ext(filename))`) against the
// stored pattern. Empty ext never matches — that lets callers treat
// "no filename signal" symmetrically with the MIME side.
func (p extPattern) matches(ext string) bool {
	if ext == "" {
		return false
	}
	return p.ext == ext
}
