package mimedetect_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ctolon/owui-cee-proxy/internal/engine/mimedetect"
)

// pdfHead is the 8-byte PDF magic prefix + a couple of token bytes —
// long enough for mimetype.Detect to recognise it as application/pdf
// without bringing the whole PDF binary into the test corpus.
var pdfHead = []byte("%PDF-1.7\n%\xe2\xe3\xcf\xd3\n")

// pngHead is the canonical PNG signature.
var pngHead = []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}

func TestResolve_DeclaredWinsWhenSpecific(t *testing.T) {
	t.Parallel()
	got := mimedetect.Resolve("application/pdf", pngHead /* deliberately mismatched */)
	require.Equal(t, mimedetect.SourceDeclared, got.Source,
		"a specific declared Content-Type MUST win over magic-byte sniffing")
	require.Equal(t, "application/pdf", got.MIME)
}

func TestResolve_DeclaredOctetStreamSniffsPDF(t *testing.T) {
	t.Parallel()
	got := mimedetect.Resolve("application/octet-stream", pdfHead)
	require.Equal(t, mimedetect.SourceSniffed, got.Source,
		"generic declared MUST trigger a sniff and adopt the more specific MIME")
	require.Equal(t, "application/pdf", got.MIME)
}

func TestResolve_EmptyDeclaredSniffsPNG(t *testing.T) {
	t.Parallel()
	got := mimedetect.Resolve("", pngHead)
	require.Equal(t, mimedetect.SourceSniffed, got.Source)
	require.Equal(t, "image/png", got.MIME)
}

func TestResolve_BinaryOctetStreamAlsoSniffs(t *testing.T) {
	t.Parallel()
	// Some legacy HTTP clients send "binary/octet-stream" — same
	// intent as application/octet-stream; both must trigger sniff.
	got := mimedetect.Resolve("binary/octet-stream", pdfHead)
	require.Equal(t, mimedetect.SourceSniffed, got.Source)
	require.Equal(t, "application/pdf", got.MIME)
}

func TestResolve_StripsParametersFromDeclared(t *testing.T) {
	t.Parallel()
	got := mimedetect.Resolve("application/pdf; charset=binary; name=file.pdf", nil)
	require.Equal(t, "application/pdf", got.MIME,
		"parameters MUST be stripped — engine routing decides on the bare media type")
	require.Equal(t, mimedetect.SourceDeclared, got.Source)
}

func TestResolve_TextPlainPreservedNotOverridden(t *testing.T) {
	t.Parallel()
	// text/plain is a real routable type (Tika, Kreuzberg), so a
	// client that explicitly sends it gets to keep it even when the
	// body would sniff as text/markdown or similar. The trust-the-
	// client invariant.
	got := mimedetect.Resolve("text/plain", []byte("# Markdown heading\n\nhello"))
	require.Equal(t, mimedetect.SourceDeclared, got.Source)
	require.Equal(t, "text/plain", got.MIME)
}

func TestResolve_SniffFailsFallback(t *testing.T) {
	t.Parallel()
	// Empty head + generic declared → no signal anywhere; we have to
	// fall back to a deterministic generic so downstream routing
	// doesn't see "".
	got := mimedetect.Resolve("application/octet-stream", nil)
	require.Equal(t, mimedetect.SourceFallback, got.Source)
	require.Equal(t, "application/octet-stream", got.MIME)
}

func TestResolve_NilHeadGenericDeclaredFallback(t *testing.T) {
	t.Parallel()
	got := mimedetect.Resolve("", nil)
	require.Equal(t, mimedetect.SourceFallback, got.Source)
	require.Equal(t, "application/octet-stream", got.MIME)
}

// TestResolve_CaseInsensitiveDeclared exercises the canonical()
// lowercaser. Real-world clients sometimes send "Application/PDF"
// from Windows-y tooling; we must canonicalise so the registry
// MIME allowlist (which stores lowercase patterns) matches.
func TestResolve_CaseInsensitiveDeclared(t *testing.T) {
	t.Parallel()
	got := mimedetect.Resolve("Application/PDF", nil)
	require.Equal(t, "application/pdf", got.MIME)
	require.Equal(t, mimedetect.SourceDeclared, got.Source)
}
