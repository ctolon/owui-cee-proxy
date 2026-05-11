package app

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/ctolon/owui-cee-proxy/internal/breaker"
	"github.com/ctolon/owui-cee-proxy/internal/config"
	"github.com/ctolon/owui-cee-proxy/internal/httpclient"
)

// TestLogEngineBootstrap_EmitsEnabledEnginesOnly is the contract pin
// for the operator-facing startup audit log. Each enabled engine
// gets one engine_initialised line; disabled engines stay silent.
// Asymmetric auth configurations (auth_header set + api_key_env
// empty) surface as engine_auth_warning lines so silent 401s in
// production are caught at boot.
func TestLogEngineBootstrap_EmitsEnabledEnginesOnly(t *testing.T) {
	t.Parallel()

	buf := &bytes.Buffer{}
	logger := zerolog.New(buf).Level(zerolog.InfoLevel).With().Timestamp().Logger()

	cfg := &config.Config{
		Engines: map[string]config.EngineConfig{
			"main": {
				Enable:     true,
				CompatType: config.CompatDocling,
				URL:        "http://docling:5001",
				AuthHeader: "X-Api-Key",
				AuthScheme: config.AuthSchemeRaw,
				APIKey:     config.Secret("secret"),
				APIKeyEnv:  "DOCLING_API_KEY",
			},
			"silent_misconfig": {
				Enable:     true,
				CompatType: config.CompatDocling,
				URL:        "http://other:5001",
				AuthHeader: "X-Api-Key",
				APIKeyEnv:  "MISSING_ENV", // not resolved
			},
			"disabled": {
				Enable:     false,
				CompatType: config.CompatDocling,
				URL:        "http://nowhere:5001",
				AuthHeader: "X-Api-Key",
			},
		},
	}

	logEngineBootstrap(logger, cfg)

	// Parse each line; classify by event.
	var inits []map[string]any
	var warnings []map[string]any
	for _, line := range bytes.Split(buf.Bytes(), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var m map[string]any
		require.NoError(t, json.Unmarshal(line, &m), "log line: %s", string(line))
		switch m["event"] {
		case "engine_initialised":
			inits = append(inits, m)
		case "engine_auth_warning":
			warnings = append(warnings, m)
		}
	}

	require.Len(t, inits, 2,
		"only enabled engines should produce engine_initialised events")
	names := []string{
		inits[0]["engine"].(string),
		inits[1]["engine"].(string),
	}
	require.ElementsMatch(t, []string{"main", "silent_misconfig"}, names)

	// silent_misconfig must have an auth warning.
	require.NotEmpty(t, warnings, "asymmetric auth config must produce a warning")
}

// TestStatusValuesAdapter_NopOnEmptyMetrics defends against panic
// when the metrics struct is partially constructed (collector nil).
// All adapters here are nil-safe by design — pin it.
func TestStatusValuesAdapter_NopOnEmptyMetrics(t *testing.T) {
	t.Parallel()
	require.NotPanics(t, func() {
		(&taskLifecycleAdapter{m: nil}).RecordTaskLifecycle("enqueued", "success")
		(&queueDepthAdapter{m: nil}).SetQueueDepth("default", 7)
		(&healthMetricsAdapter{m: nil}).RecordHealthProbe("e", "ok", time.Millisecond)
		(&authMetricsAdapter{m: nil}).RecordAuthAttempt("e", "raw", "attached")
		(&upstreamStatusAdapter{m: nil}).RecordUpstreamStatus("e", 200)
		(&ssrfMetricsAdapter{m: nil}).RecordRejection("metadata_ip")
	})
}

// TestCompatTypesAndAcceptedFacadesAlignWithAdapterCapabilities
// pins C-27: the per-compat_type facade mapping has two sources of
// truth — `config.AcceptedFacades(compat)` (used by validator + log
// bootstrap) and `adapter.Capabilities().Facades` (used by registry
// dispatch). They MUST agree. This regression test instantiates
// every registered adapter and asserts the two views match.
//
// Adding a new compat_type without updating BOTH sources will fail
// this test at the next CI run — the drift the agent flagged is
// closed by gating, not by code unification (kept the SoT split
// because they serve different lifecycle stages).
func TestCompatTypesAndAcceptedFacadesAlignWithAdapterCapabilities(t *testing.T) {
	t.Parallel()
	c, err := httpclient.New(config.EngineConfig{
		URL:            "http://stub.example",
		RequestTimeout: 1 * time.Second,
	})
	if err != nil {
		t.Fatalf("httpclient: %v", err)
	}
	br := breaker.New("stub", config.BreakerConfig{
		MaxRequests:                  1,
		Interval:                     1 * time.Second,
		Timeout:                      1 * time.Second,
		ConsecutiveFailuresThreshold: 1,
	}, nil)

	for _, ct := range config.CompatTypes() {
		ct := ct
		t.Run(string(ct), func(t *testing.T) {
			t.Parallel()
			ec := config.EngineConfig{
				CompatType: ct,
				URL:        "http://stub.example",
			}
			ad, err := newAdapterByCompat("stub", ec, c, br)
			if err != nil {
				t.Fatalf("newAdapterByCompat(%s): %v", ct, err)
			}
			gotCaps := ad.Capabilities().Facades
			want := config.AcceptedFacades(ct)
			if len(gotCaps) != len(want) {
				t.Fatalf("compat=%s: adapter facades %v, config.AcceptedFacades %v",
					ct, gotCaps, want)
			}
			gotStr := make(map[string]bool, len(gotCaps))
			for _, f := range gotCaps {
				gotStr[string(f)] = true
			}
			for _, f := range want {
				if !gotStr[f] {
					t.Errorf("compat=%s: config.AcceptedFacades has %q but adapter Capabilities does not",
						ct, f)
				}
			}
		})
	}
}
