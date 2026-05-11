// Package compatutil bundles the small string/URL/filename helpers that
// every compat adapter (docling, external, tika, doclingexternal) needs
// to construct outbound requests. Before this package existed each
// adapter shipped its own near-identical copy and a fix in one didn't
// reach the others.
package compatutil

import (
	"fmt"
	"net/url"
	"strings"
)

// JoinURL appends p to base, ensuring exactly one slash between them
// and preserving any path prefix already present on base (so reverse-
// proxied deployments that mount the engine under a sub-path still
// work). Returns an error if base is not a valid URL.
//
// Examples:
//
//	JoinURL("http://docling:5001", "/v1/convert/file")
//	  → "http://docling:5001/v1/convert/file"
//	JoinURL("http://docling:5001/api/", "v1/convert/file")
//	  → "http://docling:5001/api/v1/convert/file"
func JoinURL(base, p string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("invalid url: %w", err)
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	u.Path = strings.TrimRight(u.Path, "/") + p
	return u.String(), nil
}
