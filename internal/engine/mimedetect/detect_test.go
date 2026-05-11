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
	got := mimedetect.Resolve("application/pdf", pngHead /* deliberately mismatched */, "")
	require.Equal(t, mimedetect.SourceDeclared, got.Source,
		"a specific declared Content-Type MUST win over magic-byte sniffing")
	require.Equal(t, "application/pdf", got.MIME)
}

func TestResolve_DeclaredOctetStreamSniffsPDF(t *testing.T) {
	t.Parallel()
	got := mimedetect.Resolve("application/octet-stream", pdfHead, "")
	require.Equal(t, mimedetect.SourceSniffed, got.Source,
		"generic declared MUST trigger a sniff and adopt the more specific MIME")
	require.Equal(t, "application/pdf", got.MIME)
}

func TestResolve_EmptyDeclaredSniffsPNG(t *testing.T) {
	t.Parallel()
	got := mimedetect.Resolve("", pngHead, "")
	require.Equal(t, mimedetect.SourceSniffed, got.Source)
	require.Equal(t, "image/png", got.MIME)
}

func TestResolve_BinaryOctetStreamAlsoSniffs(t *testing.T) {
	t.Parallel()
	// Some legacy HTTP clients send "binary/octet-stream" — same
	// intent as application/octet-stream; both must trigger sniff.
	got := mimedetect.Resolve("binary/octet-stream", pdfHead, "")
	require.Equal(t, mimedetect.SourceSniffed, got.Source)
	require.Equal(t, "application/pdf", got.MIME)
}

func TestResolve_StripsParametersFromDeclared(t *testing.T) {
	t.Parallel()
	got := mimedetect.Resolve("application/pdf; charset=binary; name=file.pdf", nil, "")
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
	got := mimedetect.Resolve("text/plain", []byte("# Markdown heading\n\nhello"), "")
	require.Equal(t, mimedetect.SourceDeclared, got.Source)
	require.Equal(t, "text/plain", got.MIME)
}

func TestResolve_SniffFailsFallback(t *testing.T) {
	t.Parallel()
	// Empty head + generic declared + no filename → no signal
	// anywhere; we fall back to a deterministic generic so
	// downstream routing doesn't see "".
	got := mimedetect.Resolve("application/octet-stream", nil, "")
	require.Equal(t, mimedetect.SourceFallback, got.Source)
	require.Equal(t, "application/octet-stream", got.MIME)
}

func TestResolve_NilHeadGenericDeclaredFallback(t *testing.T) {
	t.Parallel()
	got := mimedetect.Resolve("", nil, "")
	require.Equal(t, mimedetect.SourceFallback, got.Source)
	require.Equal(t, "application/octet-stream", got.MIME)
}

// TestResolve_CaseInsensitiveDeclared exercises the canonical()
// lowercaser. Real-world clients sometimes send "Application/PDF"
// from Windows-y tooling; we must canonicalise so the registry
// MIME allowlist (which stores lowercase patterns) matches.
func TestResolve_CaseInsensitiveDeclared(t *testing.T) {
	t.Parallel()
	got := mimedetect.Resolve("Application/PDF", nil, "")
	require.Equal(t, "application/pdf", got.MIME)
	require.Equal(t, mimedetect.SourceDeclared, got.Source)
}

// cfbHead is the 8-byte Compound File Binary signature
// (D0CF11E0A1B11AE1) every legacy Office container and Outlook .msg
// file shares. mimetype.Detect collapses any such header to
// `application/vnd.ms-powerpoint` because PowerPoint was the first
// CFB family member it learned. The Resolver must disambiguate that
// with the filename extension — that's the .msg fix in action.
var cfbHead = []byte{
	0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1,
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
}

// TestResolve_CFBAmbiguityDisambiguatedByMsgExtension is the .msg
// bug fix regression test. Before this change, the proxy routed .msg
// uploads to PowerPoint-claiming engines (because the sniff returned
// application/vnd.ms-powerpoint for the shared CFB header) and got
// HTTP 400 back from the backend.
func TestResolve_CFBAmbiguityDisambiguatedByMsgExtension(t *testing.T) {
	t.Parallel()
	got := mimedetect.Resolve("application/octet-stream", cfbHead, "report.msg")
	require.Equal(t, mimedetect.SourceSniffedThenExtension, got.Source,
		"CFB-ambiguous sniff MUST be disambiguated by .msg extension")
	require.Equal(t, "application/vnd.ms-outlook", got.MIME)
}

// TestResolve_CFBAmbiguityNoFilenamePreservesSniff exercises the
// "operator declines to send filename, what happens?" path. Without
// a filename hint we keep the (ambiguous) sniff result; routing
// will either treat it as routable (engine declares it explicitly
// in mime_types) or fall back to the default engine. The behaviour
// MUST be deterministic — never crash, never fall back to octet-
// stream when the sniff produced something.
//
// We assert the result lives in the CFB-ambiguous set rather than
// pinning a single literal — different mimetype-lib versions answer
// with different members of the same family for this minimal head
// (some say application/vnd.ms-powerpoint, others application/x-ole-
// storage). The Resolver MUST preserve whichever the lib produced.
func TestResolve_CFBAmbiguityNoFilenamePreservesSniff(t *testing.T) {
	t.Parallel()
	got := mimedetect.Resolve("application/octet-stream", cfbHead, "")
	require.Equal(t, mimedetect.SourceSniffed, got.Source)
	require.Contains(t, []string{
		"application/vnd.ms-powerpoint",
		"application/vnd.ms-office",
		"application/x-cfb",
		"application/x-ole-storage",
		"application/msword",
		"application/vnd.ms-excel",
	}, got.MIME, "no-filename CFB sniff must surface as a CFB-family MIME")
}

// TestResolve_GenericSniffFailExtensionFallback locks down phase 4:
// when nothing else produces a routable type, the filename extension
// alone is allowed to drive the decision.
func TestResolve_GenericSniffFailExtensionFallback(t *testing.T) {
	t.Parallel()
	// Empty head → sniff fails entirely. Filename map hits → use it.
	got := mimedetect.Resolve("application/octet-stream", nil, "report.msg")
	require.Equal(t, mimedetect.SourceExtension, got.Source)
	require.Equal(t, "application/vnd.ms-outlook", got.MIME)
}

// TestResolve_NonAmbiguousSniffNotOverriddenByExtension protects the
// invariant that a CONFIDENT magic-byte sniff trumps the filename.
// Operators rename "report.pdf" to "report.msg" all the time; we'd
// rather route by the contents than by the user's renamed extension.
func TestResolve_NonAmbiguousSniffNotOverriddenByExtension(t *testing.T) {
	t.Parallel()
	got := mimedetect.Resolve("application/octet-stream", pdfHead, "report.msg")
	require.Equal(t, mimedetect.SourceSniffed, got.Source,
		"concrete (non-CFB) sniff MUST win over filename extension")
	require.Equal(t, "application/pdf", got.MIME)
}

// TestResolve_AllCFBExtensions is a small matrix over the four CFB
// family members in the built-in map. Each one MUST disambiguate to
// the correct MIME when paired with the shared CFB sniff result.
func TestResolve_AllCFBExtensions(t *testing.T) {
	t.Parallel()
	cases := []struct {
		filename string
		wantMIME string
	}{
		{"a.msg", "application/vnd.ms-outlook"},
		{"a.doc", "application/msword"},
		{"a.xls", "application/vnd.ms-excel"},
		{"a.ppt", "application/vnd.ms-powerpoint"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.filename, func(t *testing.T) {
			t.Parallel()
			got := mimedetect.Resolve("application/octet-stream", cfbHead, c.filename)
			require.Equal(t, c.wantMIME, got.MIME)
			require.Equal(t, mimedetect.SourceSniffedThenExtension, got.Source)
		})
	}
}

// TestResolve_ExtensionLookupCaseInsensitive — operators / clients
// upload "Report.MSG" all the time on Windows. Extension matching
// must be case-insensitive so the routing decision isn't surprising.
func TestResolve_ExtensionLookupCaseInsensitive(t *testing.T) {
	t.Parallel()
	got := mimedetect.Resolve("application/octet-stream", cfbHead, "Report.MSG")
	require.Equal(t, mimedetect.SourceSniffedThenExtension, got.Source)
	require.Equal(t, "application/vnd.ms-outlook", got.MIME)
}

// TestNewResolver_OverridesMergeWithBuiltIn — operator adds a new
// extension that doesn't shadow anything built-in. Both the new
// override and the existing built-in must remain reachable.
func TestNewResolver_OverridesMergeWithBuiltIn(t *testing.T) {
	t.Parallel()
	r := mimedetect.NewResolver(map[string]string{
		".epub": "application/epub+zip",
	})
	// New override entry visible.
	got := r.Resolve("application/octet-stream", nil, "book.epub")
	require.Equal(t, mimedetect.SourceExtension, got.Source)
	require.Equal(t, "application/epub+zip", got.MIME)
	// Built-in entry still intact.
	got = r.Resolve("application/octet-stream", nil, "a.msg")
	require.Equal(t, "application/vnd.ms-outlook", got.MIME)
}

// TestNewResolver_OverridesCanReplaceBuiltIn — operator override on
// a built-in key MUST win. Use case: an operator who deploys a
// proprietary .msg variant served by a different engine wants
// to remap the extension.
func TestNewResolver_OverridesCanReplaceBuiltIn(t *testing.T) {
	t.Parallel()
	r := mimedetect.NewResolver(map[string]string{
		".msg": "application/x-custom-msg",
	})
	got := r.Resolve("application/octet-stream", cfbHead, "a.msg")
	require.Equal(t, "application/x-custom-msg", got.MIME)
}
