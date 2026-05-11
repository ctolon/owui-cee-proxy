package authutil_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ctolon/owui-cee-proxy/internal/config"
	"github.com/ctolon/owui-cee-proxy/internal/engine/authutil"
)

// recordingMetrics captures every Apply call so tests can assert on
// the outcome counter even without standing up a full Prometheus
// registry.
type recordingMetrics struct {
	calls []call
}

type call struct {
	engine, scheme string
	outcome        authutil.Outcome
}

func (r *recordingMetrics) RecordAuthAttempt(eng, scheme string, o authutil.Outcome) {
	r.calls = append(r.calls, call{eng, scheme, o})
}

func TestApply_TableDriven(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		cfg         config.EngineConfig
		wantHeader  string
		wantValue   string
		wantOutcome authutil.Outcome
	}{
		{
			name:        "raw with key attaches literal",
			cfg:         config.EngineConfig{APIKey: "k1", AuthHeader: "X-Api-Key", AuthScheme: "raw"},
			wantHeader:  "X-Api-Key",
			wantValue:   "k1",
			wantOutcome: authutil.OutcomeAttached,
		},
		{
			name:        "bearer prepends Bearer prefix",
			cfg:         config.EngineConfig{APIKey: "k1", AuthHeader: "Authorization", AuthScheme: "bearer"},
			wantHeader:  "Authorization",
			wantValue:   "Bearer k1",
			wantOutcome: authutil.OutcomeAttached,
		},
		{
			name:        "missing key records missing_key (header set, no value)",
			cfg:         config.EngineConfig{AuthHeader: "X-Api-Key", AuthScheme: "raw"},
			wantOutcome: authutil.OutcomeMissingKey,
		},
		{
			name:        "key without header records missing_key",
			cfg:         config.EngineConfig{APIKey: "k1", AuthScheme: "raw"},
			wantOutcome: authutil.OutcomeMissingKey,
		},
		{
			name:        "neither configured records no_auth",
			cfg:         config.EngineConfig{},
			wantOutcome: authutil.OutcomeNoAuth,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := http.Header{}
			rec := &recordingMetrics{}
			got := authutil.Apply(h, "eng", tc.cfg, rec)
			require.Equal(t, tc.wantOutcome, got, "Outcome mismatch")
			if tc.wantHeader == "" {
				require.Empty(t, h, "no header expected, got %+v", h)
			} else {
				require.Equal(t, tc.wantValue, h.Get(tc.wantHeader))
			}
			require.Len(t, rec.calls, 1, "metric must be recorded exactly once")
			require.Equal(t, tc.wantOutcome, rec.calls[0].outcome)
			require.Equal(t, "eng", rec.calls[0].engine)
		})
	}
}

// TestApply_NopMetricsIsSafe pins that the Nop sentinel is wired by
// default-tested code paths (contract tests, unit tests that don't
// inject metrics). A regression that panics on nil metrics would
// take down every adapter test in the project.
func TestApply_NopMetricsIsSafe(t *testing.T) {
	t.Parallel()
	require.NotPanics(t, func() {
		authutil.Apply(http.Header{}, "eng", config.EngineConfig{
			APIKey: "k1", AuthHeader: "X-Api-Key", AuthScheme: "raw",
		}, authutil.Nop{})
	})
}

// TestApplyMulti_StampsAllConfiguredHeaders is the multi-stamp
// happy path: singular AuthHeader + AuthHeaders entries all land on
// the outbound request with the right scheme rendering.
func TestApplyMulti_StampsAllConfiguredHeaders(t *testing.T) {
	t.Parallel()
	rec := &recordingMetrics{}
	h := make(http.Header)
	cfg := config.EngineConfig{
		AuthHeader: "X-Api-Key",
		APIKey:     "primary-key",
		AuthScheme: config.AuthSchemeRaw,
		AuthHeaders: []config.AuthHeaderConfig{
			{Header: "Authorization", APIKey: "bearer-key", Scheme: config.AuthSchemeBearer},
			{Header: "X-Tenant-Token", APIKey: "tenant-1", Scheme: config.AuthSchemeRaw},
		},
	}

	outcomes := authutil.ApplyMulti(h, "kreuzberg", cfg, rec)

	require.Equal(t, []authutil.Outcome{
		authutil.OutcomeAttached, authutil.OutcomeAttached, authutil.OutcomeAttached,
	}, outcomes)
	require.Equal(t, "primary-key", h.Get("X-Api-Key"))
	require.Equal(t, "Bearer bearer-key", h.Get("Authorization"))
	require.Equal(t, "tenant-1", h.Get("X-Tenant-Token"))
	require.Len(t, rec.calls, 3, "metrics emitted once per stamp")
}

// TestApplyMulti_EmptyKeyInOneEntrySkipsOnlyThatOne pins down the
// fail-closed semantics. One empty key MUST NOT prevent the others
// from being attached — operators rotate keys one at a time.
func TestApplyMulti_EmptyKeyInOneEntrySkipsOnlyThatOne(t *testing.T) {
	t.Parallel()
	rec := &recordingMetrics{}
	h := http.Header{}
	cfg := config.EngineConfig{
		AuthHeaders: []config.AuthHeaderConfig{
			{Header: "X-Api-Key", APIKey: "key-a", Scheme: config.AuthSchemeRaw},
			{Header: "X-Missing", APIKey: "" /* rotated out */, Scheme: config.AuthSchemeRaw},
			{Header: "X-Other", APIKey: "key-b", Scheme: config.AuthSchemeRaw},
		},
	}

	outcomes := authutil.ApplyMulti(h, "kb", cfg, rec)

	require.Equal(t, []authutil.Outcome{
		authutil.OutcomeAttached, authutil.OutcomeMissingKey, authutil.OutcomeAttached,
	}, outcomes)
	require.Equal(t, "key-a", h.Get("X-Api-Key"))
	require.Empty(t, h.Get("X-Missing"))
	require.Equal(t, "key-b", h.Get("X-Other"))
}

// TestApplyMulti_NoAuthConfiguredRecordsSingleNoAuth — public engine
// path: no singular fields, no AuthHeaders. One OutcomeNoAuth
// emitted; no header touched.
func TestApplyMulti_NoAuthConfiguredRecordsSingleNoAuth(t *testing.T) {
	t.Parallel()
	rec := &recordingMetrics{}
	h := http.Header{}
	outcomes := authutil.ApplyMulti(h, "public", config.EngineConfig{}, rec)
	require.Equal(t, []authutil.Outcome{authutil.OutcomeNoAuth}, outcomes)
	require.Empty(t, h)
	require.Len(t, rec.calls, 1)
}

// TestFormatHeaderValuesMulti_BuildsWireMap covers the passthrough
// composition path: reverseproxy.New takes the rendered map.
func TestFormatHeaderValuesMulti_BuildsWireMap(t *testing.T) {
	t.Parallel()
	cfg := config.EngineConfig{
		AuthHeader: "X-Api-Key",
		APIKey:     "k",
		AuthScheme: config.AuthSchemeRaw,
		AuthHeaders: []config.AuthHeaderConfig{
			{Header: "Authorization", APIKey: "b", Scheme: config.AuthSchemeBearer},
			{Header: "X-Missing"},
		},
	}
	got := authutil.FormatHeaderValuesMulti(cfg)
	require.Equal(t, "k", got["X-Api-Key"])
	require.Equal(t, "Bearer b", got["Authorization"])
	require.NotContains(t, got, "X-Missing", "entries with empty key MUST NOT appear")
}

// TestFormatHeaderValue covers the passthrough composition. The
// reverseproxy mount can't call Apply (no http.Header at construction
// time) so it formats the value via this helper; the two must produce
// identical wire output for the same config.
func TestFormatHeaderValue(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  config.EngineConfig
		want string
	}{
		{"raw", config.EngineConfig{APIKey: "k1", AuthHeader: "X-Api-Key", AuthScheme: "raw"}, "k1"},
		{"bearer", config.EngineConfig{APIKey: "k1", AuthHeader: "Authorization", AuthScheme: "bearer"}, "Bearer k1"},
		{"empty key", config.EngineConfig{AuthHeader: "X-Api-Key"}, ""},
		{"empty header", config.EngineConfig{APIKey: "k1"}, ""},
		{"unknown scheme falls back to raw", config.EngineConfig{APIKey: "k1", AuthHeader: "X", AuthScheme: "weird"}, "k1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, authutil.FormatHeaderValue(tc.cfg))
		})
	}
}
