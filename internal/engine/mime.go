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
