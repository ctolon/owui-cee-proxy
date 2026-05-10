package docling

import "testing"

// TestSafeMultipartFilename covers H5: CRLF-laced filenames must be
// rejected (returned empty so the caller substitutes "upload"). ASCII
// filenames pass through with quote/backslash escaping.
func TestSafeMultipartFilename(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want string
	}{
		{"ok.pdf", "ok.pdf"},
		{"a\r\nb.pdf", ""}, // CRLF injection
		{"a\nb.pdf", ""},   // LF
		{"a\rb.pdf", ""},   // CR
		{"a\tb.pdf", ""},   // tab is < 0x20
		{"a\x00b.pdf", ""}, // NUL
		{"naïve.pdf", ""},  // non-ASCII rejected
		{`with "quote".pdf`, `with \"quote\".pdf`},
		{`back\slash.pdf`, `back\\slash.pdf`},
		{"", ""},
		{"file with space.pdf", "file with space.pdf"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.in, func(t *testing.T) {
			t.Parallel()
			got := safeMultipartFilename(c.in)
			if got != c.want {
				t.Fatalf("safeMultipartFilename(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
