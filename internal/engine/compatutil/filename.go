package compatutil

import "strings"

// SafeFilename returns name unchanged when it contains only printable
// ASCII bytes (0x20..0x7E). Anything outside that range — including
// CR/LF/NUL bytes that would corrupt a multipart header or an X-
// Filename header — causes the function to return the empty string.
// Callers substitute a placeholder ("upload") in that case.
//
// This is the validating variant used by the external (raw PUT)
// path. The docling multipart path needs an additional layer of
// quoting; use SafeMultipartFilename for that.
func SafeFilename(name string) string {
	for i := 0; i < len(name); i++ {
		b := name[i]
		if b < 0x20 || b > 0x7E {
			return ""
		}
	}
	return name
}

// SafeMultipartFilename is SafeFilename + RFC 7578-style escaping for
// embedding the filename inside a Content-Disposition header value.
// Backslashes and double-quotes are escaped so a hostile filename
// can't break out of the surrounding `filename="…"` token.
func SafeMultipartFilename(name string) string {
	if SafeFilename(name) == "" {
		return ""
	}
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(name)
}
