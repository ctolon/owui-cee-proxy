package engine

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// routingFixture builds a 3-engine registry for the routing tests:
//
//	default-eng — accepts both facades, owns the catch-all fallback.
//	pdf-eng     — MIME claim on application/pdf; no extension claim.
//	msg-eng     — Extension claim on .msg; no MIME claim.
//
// The asymmetric setup lets every test pin down which phase made the
// decision: MIME hits go to pdf-eng, extension hits go to msg-eng,
// fall-throughs go to default-eng.
func routingFixture(t *testing.T, strategy RoutingStrategy) (Registry, *fakeEngine, *fakeEngine, *fakeEngine) {
	t.Helper()
	d := &fakeEngine{name: "default-eng", caps: bothFacades()}
	pdf := &fakeEngine{name: "pdf-eng", caps: bothFacades()}
	msg := &fakeEngine{name: "msg-eng", caps: bothFacades()}
	r, err := NewRegistry(map[Name]RegistryEntry{
		"default-eng": {Engine: d},
		"pdf-eng":     {Engine: pdf, MimeTypes: []string{"application/pdf"}},
		"msg-eng":     {Engine: msg, Extensions: []string{".msg"}},
	}, "default-eng", strategy)
	require.NoError(t, err)
	return r, d, pdf, msg
}

// TestPickRoute_AllFourStrategies pins down the dispatch matrix for
// every strategy × signal combination. Adding a new strategy means
// adding a new column to this matrix — that's the entire point of
// keeping the table dense.
func TestPickRoute_AllFourStrategies(t *testing.T) {
	t.Parallel()

	type want struct {
		engine string
		source PickSource
	}
	cases := []struct {
		name     string
		strategy RoutingStrategy
		hint     PickHint
		want     want
	}{
		// Strategy: mime — extension hint must be ignored.
		{"mime/pdf-mime-hits", StrategyMime, PickHint{MIME: "application/pdf"}, want{"pdf-eng", PickSourceMIME}},
		{"mime/msg-ext-ignored", StrategyMime, PickHint{Filename: "a.msg"}, want{"default-eng", PickSourceDefault}},
		{"mime/no-signal", StrategyMime, PickHint{}, want{"default-eng", PickSourceDefault}},

		// Strategy: extension — mime hint must be ignored.
		{"extension/msg-ext-hits", StrategyExtension, PickHint{Filename: "a.msg"}, want{"msg-eng", PickSourceExtension}},
		{"extension/pdf-mime-ignored", StrategyExtension, PickHint{MIME: "application/pdf"}, want{"default-eng", PickSourceDefault}},
		{"extension/no-signal", StrategyExtension, PickHint{}, want{"default-eng", PickSourceDefault}},

		// Strategy: mime_then_extension — mime first; fall back to ext.
		{"mte/mime-wins", StrategyMimeThenExt, PickHint{MIME: "application/pdf", Filename: "a.msg"}, want{"pdf-eng", PickSourceMIME}},
		{"mte/mime-misses-ext-hits", StrategyMimeThenExt, PickHint{MIME: "text/plain", Filename: "a.msg"}, want{"msg-eng", PickSourceExtension}},
		{"mte/both-miss", StrategyMimeThenExt, PickHint{MIME: "text/plain", Filename: "a.bin"}, want{"default-eng", PickSourceDefault}},
		{"mte/no-signal", StrategyMimeThenExt, PickHint{}, want{"default-eng", PickSourceDefault}},

		// Strategy: extension_then_mime — ext first; fall back to mime.
		{"etm/ext-wins", StrategyExtThenMime, PickHint{MIME: "application/pdf", Filename: "a.msg"}, want{"msg-eng", PickSourceExtension}},
		{"etm/ext-misses-mime-hits", StrategyExtThenMime, PickHint{MIME: "application/pdf", Filename: "a.bin"}, want{"pdf-eng", PickSourceMIME}},
		{"etm/both-miss", StrategyExtThenMime, PickHint{MIME: "text/plain", Filename: "a.bin"}, want{"default-eng", PickSourceDefault}},
		{"etm/no-signal", StrategyExtThenMime, PickHint{}, want{"default-eng", PickSourceDefault}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r, _, _, _ := routingFixture(t, c.strategy)
			eng, src, err := r.PickRouteVerbose(FacadeDocling, c.hint)
			require.NoError(t, err)
			require.Equal(t, c.want.engine, string(eng.Name()), "engine mismatch")
			require.Equal(t, c.want.source, src, "pick source mismatch")
		})
	}
}

// TestPickRoute_DefaultStrategyResolvesToMimeThenExt asserts the
// zero-value strategy still preserves v0.4.x behaviour when an engine
// has only a MIME claim: MIME hits land on the right engine, no
// extension fallback is invoked.
func TestPickRoute_DefaultStrategyResolvesToMimeThenExt(t *testing.T) {
	t.Parallel()
	r, _, pdf, _ := routingFixture(t, "" /* empty → mime_then_extension */)
	require.Equal(t, StrategyMimeThenExt, r.Strategy())

	eng, src, err := r.PickRouteVerbose(FacadeDocling, PickHint{MIME: "application/pdf"})
	require.NoError(t, err)
	require.Same(t, pdf, eng)
	require.Equal(t, PickSourceMIME, src)
}

// TestPickRoute_DefaultIsFinalFallback exercises the "no phase
// claimed" path: every strategy must end at the default engine when
// the request signals don't match any non-default engine.
func TestPickRoute_DefaultIsFinalFallback(t *testing.T) {
	t.Parallel()
	for _, s := range []RoutingStrategy{StrategyMime, StrategyExtension, StrategyMimeThenExt, StrategyExtThenMime} {
		t.Run(string(s), func(t *testing.T) {
			t.Parallel()
			r, d, _, _ := routingFixture(t, s)
			eng, src, err := r.PickRouteVerbose(FacadeDocling, PickHint{MIME: "audio/mpeg", Filename: "a.unknown"})
			require.NoError(t, err)
			require.Same(t, d, eng)
			require.Equal(t, PickSourceDefault, src)
		})
	}
}

// TestPickRoute_EmptyHintFallsThroughToDefault asserts that
// source-mode requests (no MIME, no filename) bypass every matching
// phase regardless of strategy.
func TestPickRoute_EmptyHintFallsThroughToDefault(t *testing.T) {
	t.Parallel()
	for _, s := range []RoutingStrategy{StrategyMime, StrategyExtension, StrategyMimeThenExt, StrategyExtThenMime} {
		t.Run(string(s), func(t *testing.T) {
			t.Parallel()
			r, d, _, _ := routingFixture(t, s)
			eng, src, err := r.PickRouteVerbose(FacadeDocling, PickHint{})
			require.NoError(t, err)
			require.Same(t, d, eng)
			require.Equal(t, PickSourceDefault, src)
		})
	}
}

// TestPickWithError_ShimDelegates verifies the legacy MIME-only shim
// still works after the registry was rebuilt around PickRoute.
func TestPickWithError_ShimDelegates(t *testing.T) {
	t.Parallel()
	r, _, pdf, _ := routingFixture(t, StrategyMimeThenExt)
	got, err := r.PickWithError(FacadeDocling, "application/pdf")
	require.NoError(t, err)
	require.Same(t, pdf, got)
}

// TestPick_ShimPermissiveFallback confirms the v0.2.x Pick shim still
// falls back to the default engine on no-match instead of returning
// nil — the shim is permissive by design (callers relied on a
// non-nil engine pointer).
func TestPick_ShimPermissiveFallback(t *testing.T) {
	t.Parallel()
	r, d, _, _ := routingFixture(t, StrategyMime)
	got := r.Pick(FacadeDocling, "text/plain")
	require.Same(t, d, got)
}

// TestExtensionPattern_LeadingDotNormalisation locks down the
// compileExtensions normalisation contract: "PDF", "pdf", and ".PDF"
// all collapse to the same canonical ".pdf" pattern.
func TestExtensionPattern_LeadingDotNormalisation(t *testing.T) {
	t.Parallel()
	in := []string{"PDF", "pdf", ".PDF", ".pdf", "  .Msg  ", ""}
	out := compileExtensions(in)
	require.Len(t, out, 5, "blank inputs should be dropped, others normalised")
	for _, p := range out {
		require.True(t, p.ext == ".pdf" || p.ext == ".msg", "unexpected ext pattern %q", p.ext)
	}
}

// TestExtensionPattern_MatchesIsCaseInsensitive confirms operators
// can declare ".PDF" in YAML and the runtime will still match a
// canonical ".pdf" extension extracted from filepath.Ext.
func TestExtensionPattern_MatchesIsCaseInsensitive(t *testing.T) {
	t.Parallel()
	pats := compileExtensions([]string{".MSG"})
	require.Len(t, pats, 1)
	require.True(t, pats[0].matches(".msg"))
	require.False(t, pats[0].matches(".doc"))
	require.False(t, pats[0].matches(""), "empty ext never matches")
}

// TestResolveStrategy_EmptyResolvesToMimeThenExt — the
// "no-config" path must produce the v0.4.x-compatible default.
func TestResolveStrategy_EmptyResolvesToMimeThenExt(t *testing.T) {
	t.Parallel()
	require.Equal(t, StrategyMimeThenExt, ResolveStrategy(""))
	require.Equal(t, StrategyMime, ResolveStrategy(StrategyMime))
	require.Equal(t, StrategyExtension, ResolveStrategy(StrategyExtension))
	require.Equal(t, StrategyMimeThenExt, ResolveStrategy(StrategyMimeThenExt))
	require.Equal(t, StrategyExtThenMime, ResolveStrategy(StrategyExtThenMime))
	// Unknown literal falls back to the safe default.
	require.Equal(t, StrategyMimeThenExt, ResolveStrategy("garbled"))
}
