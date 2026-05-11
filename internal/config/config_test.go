package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ctolon/owui-cee-proxy/internal/engine"
)

func TestDefaults(t *testing.T) {
	t.Parallel()
	c := Default()
	require.Equal(t, "0.0.0.0:8080", c.Server.Listen)
	require.Equal(t, "/v1", c.Routing.Facade.Docling.Prefix)
	require.Equal(t, "/process", c.Routing.Facade.External.Path)
	require.True(t, c.Routing.Facade.Docling.Enabled)
	require.True(t, c.Routing.Facade.External.Enabled)
	require.Equal(t, 30*time.Second, c.Server.ReadTimeout)
	require.True(t, c.Observability.Metrics.Enabled)
	require.False(t, c.Observability.Log.AddCaller)
	require.NotNil(t, c.Engines)
	require.Empty(t, c.Engines)
}

func TestLoad_ValidYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "valid.yaml")
	require.NoError(t, os.WriteFile(path, []byte(validYAML), 0o600))

	t.Setenv("DOCLING_API_KEY", "docling-secret")
	t.Setenv("KREUZBERG_API_KEY", "kreuzberg-secret")
	t.Setenv("PROXY_API_KEYS", "k1, k2 ,k3")

	c, err := Load(path)
	require.NoError(t, err)
	require.Equal(t, "main", c.Routing.DefaultEngine)
	require.Len(t, c.Engines, 2)

	main := c.Engines["main"]
	require.Equal(t, CompatDocling, main.CompatType)
	require.Equal(t, "http://docling.local:5001", main.URL)
	require.Equal(t, Secret("docling-secret"), main.APIKey)
	require.Equal(t, "X-Api-Key", main.AuthHeader)
	require.Equal(t, AuthSchemeRaw, main.AuthScheme)

	kb := c.Engines["fallback"]
	require.Equal(t, CompatDocling, kb.CompatType)
	require.Equal(t, Secret("kreuzberg-secret"), kb.APIKey)
	require.Equal(t, "Authorization", kb.AuthHeader)
	require.Equal(t, AuthSchemeBearer, kb.AuthScheme)

	require.Len(t, c.Security.ProxyAPIKeys, 3)
}

func TestLoad_EnvOverridesYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "v.yaml")
	require.NoError(t, os.WriteFile(path, []byte(validYAML), 0o600))

	t.Setenv("OWUI_PROXY_SERVER__LISTEN", "127.0.0.1:9999")
	t.Setenv("OWUI_PROXY_ENGINES__MAIN__URL", "http://override:5001")

	c, err := Load(path)
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1:9999", c.Server.Listen)
	require.Equal(t, "http://override:5001", c.Engines["main"].URL)
}

func TestValidate_DefaultEngineMustExist(t *testing.T) {
	t.Parallel()
	c := Default()
	c.Routing.DefaultEngine = "missing"
	c.Engines["main"] = EngineConfig{
		Enable:     true,
		CompatType: CompatDocling,
		URL:        "http://docling:5001",
		AuthHeader: "X-Api-Key",
	}
	require.Error(t, Validate(c))
}

func TestValidate_DefaultEngineMustBeEnabled(t *testing.T) {
	t.Parallel()
	c := Default()
	c.Routing.DefaultEngine = "main"
	c.Engines["main"] = EngineConfig{
		Enable:     false,
		CompatType: CompatDocling,
	}
	require.Error(t, Validate(c))
}

func TestValidate_FacadeCoverage(t *testing.T) {
	t.Parallel()
	c := Default()
	c.Engines["only-docling"] = EngineConfig{
		Enable:     true,
		CompatType: CompatDocling,
		URL:        "http://docling:5001",
		AuthHeader: "X-Api-Key",
	}
	c.Routing.DefaultEngine = "only-docling"
	c.Routing.Facade.External.Enabled = true
	err := Validate(c)
	require.Error(t, err, "external facade enabled but no engine answers it")
}

func TestValidate_EngineNameCharset(t *testing.T) {
	t.Parallel()
	c := Default()
	c.Engines["BAD NAME"] = EngineConfig{
		Enable:     true,
		CompatType: CompatDocling,
		URL:        "http://docling:5001",
	}
	c.Routing.DefaultEngine = "BAD NAME"
	c.Routing.Facade.External.Enabled = false
	require.Error(t, Validate(c))
}

func TestValidate_AuthHeaderCharset(t *testing.T) {
	t.Parallel()
	c := Default()
	c.Engines["main"] = EngineConfig{
		Enable:     true,
		CompatType: CompatDocling,
		URL:        "http://docling:5001",
		AuthHeader: "X-Api-Key\r\nInjected: yes",
	}
	c.Routing.DefaultEngine = "main"
	c.Routing.Facade.External.Enabled = false
	require.Error(t, Validate(c))
}

func TestValidate_TasksRequireRedis(t *testing.T) {
	t.Parallel()
	c := Default()
	c.Engines["main"] = EngineConfig{
		Enable:     true,
		CompatType: CompatDocling,
		URL:        "http://docling:5001",
		AuthHeader: "X-Api-Key",
	}
	c.Routing.DefaultEngine = "main"
	c.Routing.Facade.External.Enabled = false
	c.Tasks.Enabled = true
	c.Tasks.RedisURL = ""
	require.Error(t, Validate(c))
}

func TestValidate_TLSRequiresCertAndKey(t *testing.T) {
	t.Parallel()
	c := Default()
	c.Engines["main"] = EngineConfig{
		Enable:     true,
		CompatType: CompatDocling,
		URL:        "http://docling:5001",
		AuthHeader: "X-Api-Key",
	}
	c.Routing.DefaultEngine = "main"
	c.Routing.Facade.External.Enabled = false
	c.Server.TLS.Enabled = true
	require.Error(t, Validate(c))
}

func TestSecret_RedactsInJSON(t *testing.T) {
	t.Parallel()
	type wrapper struct {
		Token Secret `json:"token"`
	}
	b, err := json.Marshal(wrapper{Token: "supersecret"})
	require.NoError(t, err)
	require.Equal(t, `{"token":"***"}`, string(b))

	b, err = json.Marshal(wrapper{Token: ""})
	require.NoError(t, err)
	require.Equal(t, `{"token":""}`, string(b))
}

// TestResolveSecrets_MissingEnvLeavesAPIKeyEmpty pins the silent-failure
// mode the user hits when api_key_env is set but the named env var
// isn't exported into the container/Pod (e.g., Helm Secret not mounted,
// Secret key name doesn't match, pod not rolled after Secret update).
// The proxy intentionally does NOT error out — the engine may legitimately
// be public — but operators MUST be able to tell from the resulting empty
// APIKey that the wiring is broken. Without this test, a refactor that
// throws on missing env vars would silently regress the "engine without
// auth" deployment.
func TestResolveSecrets_MissingEnvLeavesAPIKeyEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "v.yaml")
	require.NoError(t, os.WriteFile(path, []byte(validYAML), 0o600))

	// Ensure the env vars are NOT set for this test (t.Setenv is unset
	// at teardown but we never set it). Explicitly Unsetenv to defeat
	// any pollution from a prior test in the same process.
	require.NoError(t, os.Unsetenv("DOCLING_API_KEY"))
	require.NoError(t, os.Unsetenv("KREUZBERG_API_KEY"))
	require.NoError(t, os.Unsetenv("PROXY_API_KEYS"))

	c, err := Load(path)
	require.NoError(t, err, "missing api_key_env env vars must not fail Load")
	require.Equal(t, Secret(""), c.Engines["main"].APIKey,
		"unresolved api_key_env must leave APIKey empty; adapters skip the auth header on empty")
	require.Equal(t, Secret(""), c.Engines["fallback"].APIKey)
	require.Empty(t, c.Security.ProxyAPIKeys,
		"unresolved proxy_api_keys_env must leave ProxyAPIKeys empty")
}

// TestResolveSecrets_NoAPIKeyEnvLeavesAPIKeyEmpty covers the OTHER
// silent path: the YAML doesn't declare api_key_env at all (matches
// deployments/docker/test-proxy-config.yaml as of v0.2.0). The proxy
// won't look up any env var, even if one is set. This is the
// configuration error most commonly reported as "I set DOCLING_API_KEY
// but the proxy isn't forwarding it" — the fix is to add
// `api_key_env: DOCLING_API_KEY` to the engine stanza.
func TestResolveSecrets_NoAPIKeyEnvLeavesAPIKeyEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "v.yaml")
	yamlNoEnv := `
server:
  listen: "0.0.0.0:8080"
routing:
  default_engine: "main"
  facade:
    docling:
      enabled: true
    external:
      enabled: false
engines:
  main:
    enable: true
    compat_type: "docling"
    url: "http://docling.local:5001"
    auth_header: "X-Api-Key"
`
	require.NoError(t, os.WriteFile(path, []byte(yamlNoEnv), 0o600))

	// Even with the env var set, the proxy should NOT pick it up
	// because the YAML never declared api_key_env.
	t.Setenv("DOCLING_API_KEY", "this-should-be-ignored")

	c, err := Load(path)
	require.NoError(t, err)
	require.Equal(t, Secret(""), c.Engines["main"].APIKey,
		"missing api_key_env in YAML means proxy never reads any env var, even if one is set")
}

// TestSecretMasking_LeakProtection guarantees that Secret values are
// never written verbatim to logs/JSON, regardless of how an adapter
// or middleware accidentally tries to format them. Defence in depth
// against an operator running with debug-level logging or dumping
// config in an incident.
func TestSecretMasking_LeakProtection(t *testing.T) {
	t.Parallel()
	s := Secret("super-secret-do-not-leak")

	require.Equal(t, "***", s.String(), "Stringer must mask")

	b, err := s.MarshalJSON()
	require.NoError(t, err)
	require.Equal(t, `"***"`, string(b))

	b2, err := s.MarshalText()
	require.NoError(t, err)
	require.Equal(t, "***", string(b2))

	// And the empty Secret stays empty (no spurious "***" when no
	// secret was configured — operators rely on the empty signal).
	require.Equal(t, "", Secret("").String())
}

// TestResolveUpstream_GlobalFallback confirms per-engine fields fall
// back to the global defaults when nil. A regression that copies the
// zero value rather than inheriting would silently disable upstream
// logging for any engine that doesn't override anything.
func TestResolveUpstream_GlobalFallback(t *testing.T) {
	t.Parallel()
	global := UpstreamLogConfig{
		Enabled: true, BodySnippetBytes: 256, Level4xx: "warn", Level5xx: "error", BodyLogBytes: 0,
	}
	ec := EngineConfig{} // no overrides
	got := ec.ResolveUpstream(global)
	require.Equal(t, global, got, "empty override must produce the global config")
}

// TestResolveUpstream_PerEngineOverride verifies each pointer-typed
// override field is honoured individually (a *bool to false MUST
// disable, even when global default is true).
func TestResolveUpstream_PerEngineOverride(t *testing.T) {
	t.Parallel()
	global := UpstreamLogConfig{
		Enabled: true, BodySnippetBytes: 256, Level4xx: "warn", Level5xx: "error",
	}
	falseV := false
	bs := 1024
	lv := "info"
	bl := 512
	ec := EngineConfig{
		Observability: EngineObservabilityConfig{
			Enabled:          &falseV,
			BodySnippetBytes: &bs,
			Level4xx:         &lv,
			BodyLogBytes:     &bl,
		},
	}
	got := ec.ResolveUpstream(global)
	require.False(t, got.Enabled, "explicit false override must win over global true")
	require.Equal(t, 1024, got.BodySnippetBytes)
	require.Equal(t, "info", got.Level4xx)
	require.Equal(t, "error", got.Level5xx, "non-overridden Level5xx must inherit global")
	require.Equal(t, 512, got.BodyLogBytes)
}

// TestAuthWarnings covers the validation surface that catches the
// user's actual silent-failure: api_key_env set but env var empty.
func TestAuthWarnings(t *testing.T) {
	t.Parallel()
	c := &Config{
		Engines: map[string]EngineConfig{
			"happy": {
				Enable: true, CompatType: CompatDocling,
				AuthHeader: "X-Api-Key", APIKeyEnv: "K", APIKey: "v",
			},
			"silent_missing_env": {
				Enable: true, CompatType: CompatDocling,
				AuthHeader: "X-Api-Key", APIKeyEnv: "K_MISSING", APIKey: "",
			},
			"no_env_var_but_header": {
				Enable: true, CompatType: CompatDocling,
				AuthHeader: "X-Api-Key", APIKeyEnv: "", APIKey: "",
			},
			"disabled_engine_skipped": {
				Enable: false, CompatType: CompatDocling,
				AuthHeader: "X-Api-Key", APIKeyEnv: "",
			},
		},
	}
	got := c.AuthWarnings()
	require.Len(t, got, 2, "exactly 2 enabled engines have asymmetric auth config")
	joined := strings.Join(got, "\n")
	require.Contains(t, joined, "silent_missing_env")
	require.Contains(t, joined, "K_MISSING")
	require.Contains(t, joined, "no_env_var_but_header")
	require.NotContains(t, joined, "disabled_engine_skipped",
		"disabled engines must not produce warnings")
	require.NotContains(t, joined, "happy", "happy path must not produce a warning")
}

func TestAcceptedFacades(t *testing.T) {
	t.Parallel()
	require.ElementsMatch(t, []string{"docling"}, AcceptedFacades(CompatDocling))
	require.ElementsMatch(t, []string{"external"}, AcceptedFacades(CompatExternal))
	require.ElementsMatch(t, []string{"docling", "external"}, AcceptedFacades(CompatDoclingExternal))
	require.ElementsMatch(t, []string{"docling", "external"}, AcceptedFacades(CompatTika))
	require.Empty(t, AcceptedFacades("unknown"))
}

// TestDefaults_RoutingStrategyIsMimeThenExtension pins down the
// operator-facing default. Any time the default changes, an
// integration test elsewhere needs to be updated, so the assertion
// stays on its own line.
func TestDefaults_RoutingStrategyIsMimeThenExtension(t *testing.T) {
	t.Parallel()
	c := Default()
	require.Equal(t, engine.StrategyMimeThenExt, c.Routing.Strategy,
		"default routing strategy MUST preserve v0.4.x MIME-only behaviour")
}

// TestLoad_RoutingStrategy_AllValues is a small driver over the four
// valid strategy literals — proves the validator accepts each and
// the value flows through Load() unmolested.
func TestLoad_RoutingStrategy_AllValues(t *testing.T) {
	for _, s := range []string{"mime", "extension", "mime_then_extension", "extension_then_mime"} {
		s := s
		t.Run(s, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "v.yaml")
			yaml := strings.Replace(validYAML, `default_engine: "main"`,
				`default_engine: "main"
  strategy: "`+s+`"`, 1)
			require.NoError(t, os.WriteFile(path, []byte(yaml), 0o600))

			t.Setenv("DOCLING_API_KEY", "a")
			t.Setenv("KREUZBERG_API_KEY", "b")

			c, err := Load(path)
			require.NoError(t, err)
			require.Equal(t, engine.RoutingStrategy(s), c.Routing.Strategy)
		})
	}
}

// TestLoad_RoutingStrategy_RejectsUnknown — the validator MUST fail
// the bootstrap on a typo'd strategy literal. Operator gets a clear
// startup error instead of a silent degradation into the default.
func TestLoad_RoutingStrategy_RejectsUnknown(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "v.yaml")
	yaml := strings.Replace(validYAML, `default_engine: "main"`,
		`default_engine: "main"
  strategy: "mim_then_ext"`, 1)
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o600))

	t.Setenv("DOCLING_API_KEY", "a")
	t.Setenv("KREUZBERG_API_KEY", "b")

	_, err := Load(path)
	require.Error(t, err)
	require.Contains(t, err.Error(), "Strategy")
}

// TestLoad_EngineExtensions_RoundTrip — operator declares an
// extensions allowlist in YAML; it MUST round-trip through Load
// without mutation so the registry sees what was written.
func TestLoad_EngineExtensions_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "v.yaml")
	yaml := strings.Replace(validYAML, `mime_types: ["application/pdf"]`,
		`mime_types: ["application/pdf"]
    extensions: [".pdf", ".PDF"]`, 1)
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o600))

	t.Setenv("DOCLING_API_KEY", "a")
	t.Setenv("KREUZBERG_API_KEY", "b")

	c, err := Load(path)
	require.NoError(t, err)
	require.Equal(t, []string{".pdf", ".PDF"}, c.Engines["main"].Extensions,
		"extensions list must round-trip verbatim; normalisation happens later in compileExtensions")
}

// TestLoad_MimedetectOverrides_RoundTrip — operator extension
// overrides MUST flow through Load and validate cleanly.
func TestLoad_MimedetectOverrides_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "v.yaml")
	yaml := validYAML + `
mimedetect:
  extension_overrides:
    ".epub": "application/epub+zip"
    ".msg": "application/x-custom-msg"
`
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o600))

	t.Setenv("DOCLING_API_KEY", "a")
	t.Setenv("KREUZBERG_API_KEY", "b")

	c, err := Load(path)
	require.NoError(t, err)
	require.Equal(t, "application/epub+zip", c.Mimedetect.ExtensionOverrides[".epub"])
	require.Equal(t, "application/x-custom-msg", c.Mimedetect.ExtensionOverrides[".msg"])
}

// TestLoad_MimedetectOverrides_RejectsBadKey — typo'd key (missing
// leading dot, uppercase, etc.) MUST fail Load.
func TestLoad_MimedetectOverrides_RejectsBadKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "v.yaml")
	yaml := validYAML + `
mimedetect:
  extension_overrides:
    "msg": "application/x-custom-msg"
`
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o600))

	t.Setenv("DOCLING_API_KEY", "a")
	t.Setenv("KREUZBERG_API_KEY", "b")

	_, err := Load(path)
	require.Error(t, err)
	require.Contains(t, err.Error(), "extension_overrides")
}

// TestValidateTimeoutHierarchy_RejectsEngineExceedingServerTimeout
// pins the C-2 invariant: an engine's request_timeout MUST NOT
// exceed server.request_timeout. The chi mw.Timeout(server.request_
// timeout) would otherwise cancel the context before the engine's
// own deadline fires, surfacing as a transport error to the breaker
// and tripping a healthy backend.
func TestValidateTimeoutHierarchy_RejectsEngineExceedingServerTimeout(t *testing.T) {
	t.Parallel()
	c := Default()
	c.Routing.DefaultEngine = "main"
	c.Routing.Facade.External.Enabled = false
	c.Server.RequestTimeout = 60 * time.Second
	c.Server.WriteTimeout = 70 * time.Second
	c.Engines["main"] = EngineConfig{
		Enable:         true,
		CompatType:     CompatDocling,
		URL:            "http://docling.local",
		AuthHeader:     "X-Api-Key",
		APIKey:         "k",
		RequestTimeout: 5 * time.Minute, // exceeds 60 s
	}
	err := Validate(c)
	require.Error(t, err)
	require.Contains(t, err.Error(), "timeout hierarchy")
	require.Contains(t, err.Error(), "engines.main.request_timeout")
}

// TestValidateTimeoutHierarchy_RejectsWriteTimeoutBelowSlack pins
// the second half of the invariant: server.write_timeout MUST be at
// least 2 s above server.request_timeout so the response writer
// does not tear down mid-stream when a slow handler exhausts its
// per-request budget.
func TestValidateTimeoutHierarchy_RejectsWriteTimeoutBelowSlack(t *testing.T) {
	t.Parallel()
	c := Default()
	c.Routing.DefaultEngine = "main"
	c.Routing.Facade.External.Enabled = false
	c.Server.RequestTimeout = 60 * time.Second
	c.Server.WriteTimeout = 60 * time.Second // no slack
	c.Engines["main"] = EngineConfig{
		Enable: true, CompatType: CompatDocling, URL: "http://docling.local",
		AuthHeader: "X-Api-Key", APIKey: "k", RequestTimeout: 30 * time.Second,
	}
	err := Validate(c)
	require.Error(t, err)
	require.Contains(t, err.Error(), "timeout hierarchy")
	require.Contains(t, err.Error(), "write_timeout")
}

// TestValidateTimeoutHierarchy_AcceptsCorrectHierarchy verifies the
// happy path: engine ≤ server.request ≤ server.write − slack.
func TestValidateTimeoutHierarchy_AcceptsCorrectHierarchy(t *testing.T) {
	t.Parallel()
	c := Default()
	c.Routing.DefaultEngine = "main"
	c.Routing.Facade.External.Enabled = false
	c.Server.RequestTimeout = 60 * time.Second
	c.Server.WriteTimeout = 5 * time.Minute
	c.Engines["main"] = EngineConfig{
		Enable: true, CompatType: CompatDocling, URL: "http://docling.local",
		AuthHeader: "X-Api-Key", APIKey: "k", RequestTimeout: 30 * time.Second,
	}
	require.NoError(t, Validate(c))
}

// TestValidateResolvedSecrets_RejectsCRLFInResolvedAPIKey pins the
// C-4 fix. An env-var value that picks up a stray CR/LF (typical
// when an operator pastes a multi-line secret) must fail the
// bootstrap, not silently smuggle a header-injection payload into
// the outbound auth header.
func TestValidateResolvedSecrets_RejectsCRLFInResolvedAPIKey(t *testing.T) {
	t.Parallel()
	c := Default()
	c.Routing.DefaultEngine = "main"
	c.Routing.Facade.External.Enabled = false // single-engine docling-only fixture
	c.Engines["main"] = EngineConfig{
		Enable:     true,
		CompatType: CompatDocling,
		URL:        "http://docling.local",
		AuthHeader: "X-Api-Key",
		APIKey:     "good-key\r\nX-Admin: yes",
	}
	err := Validate(c)
	require.Error(t, err)
	require.Contains(t, err.Error(), "non-printable byte")
	require.Contains(t, err.Error(), "engines.main.api_key")
}

// TestValidateResolvedSecrets_RejectsControlByteInMultiAuthEntry
// covers the C-4 fix on the new multi-stamp surface.
func TestValidateResolvedSecrets_RejectsControlByteInMultiAuthEntry(t *testing.T) {
	t.Parallel()
	c := Default()
	c.Routing.DefaultEngine = "main"
	c.Routing.Facade.External.Enabled = false
	c.Engines["main"] = EngineConfig{
		Enable:     true,
		CompatType: CompatDocling,
		URL:        "http://docling.local",
		AuthHeader: "X-Api-Key",
		APIKey:     "primary",
		AuthHeaders: []AuthHeaderConfig{
			{Header: "Authorization", APIKeyEnv: "X", APIKey: "bearer\x00null"},
		},
	}
	err := Validate(c)
	require.Error(t, err)
	require.Contains(t, err.Error(), "auth_headers[0]")
}

// TestValidateResolvedSecrets_AcceptsCleanASCII passes for the happy
// path — printable ASCII plus tabs (allowed because some token
// schemes use them as separators) flow through unchanged.
func TestValidateResolvedSecrets_AcceptsCleanASCII(t *testing.T) {
	t.Parallel()
	c := Default()
	c.Routing.DefaultEngine = "main"
	c.Routing.Facade.External.Enabled = false
	c.Engines["main"] = EngineConfig{
		Enable:     true,
		CompatType: CompatDocling,
		URL:        "http://docling.local",
		AuthHeader: "X-Api-Key",
		APIKey:     "ABC.123-tab\there",
	}
	require.NoError(t, Validate(c))
}

// TestEngineConfig_EffectiveAuthSpecs_PrependsSingular pins the
// migration contract: a legacy singular AuthHeader keeps working
// AND comes FIRST in the resolved spec list. AuthHeaders entries
// follow in YAML order.
func TestEngineConfig_EffectiveAuthSpecs_PrependsSingular(t *testing.T) {
	t.Parallel()
	ec := EngineConfig{
		AuthHeader: "X-Api-Key",
		APIKey:     "primary",
		AuthScheme: AuthSchemeRaw,
		AuthHeaders: []AuthHeaderConfig{
			{Header: "Authorization", APIKey: "bearer-key", Scheme: AuthSchemeBearer},
			{Header: "X-Tenant", APIKey: "tenant-1", Scheme: AuthSchemeRaw},
		},
	}
	specs := ec.EffectiveAuthSpecs()
	require.Len(t, specs, 3)
	require.Equal(t, "X-Api-Key", specs[0].Header)
	require.Equal(t, Secret("primary"), specs[0].APIKey)
	require.Equal(t, "Authorization", specs[1].Header)
	require.Equal(t, "X-Tenant", specs[2].Header)
}

// TestEngineConfig_EffectiveAuthSpecs_PublicEngineEmpty — no
// AuthHeader, no AuthHeaders → empty slice (public-engine path).
func TestEngineConfig_EffectiveAuthSpecs_PublicEngineEmpty(t *testing.T) {
	t.Parallel()
	require.Empty(t, EngineConfig{}.EffectiveAuthSpecs())
}

// TestValidate_AuthHeadersDedupAcrossSingularAndSlice locks down the
// case-insensitive dedup contract. An operator who declares the same
// header in both forms (or twice in the slice) must get a clear
// bootstrap error — silent overwrite would mask the real config drift.
func TestValidate_AuthHeadersDedupAcrossSingularAndSlice(t *testing.T) {
	t.Parallel()
	c := Default()
	c.Routing.DefaultEngine = "main"
	c.Engines["main"] = EngineConfig{
		Enable:     true,
		CompatType: CompatDocling,
		URL:        "http://docling.local",
		AuthHeader: "X-Api-Key",
		AuthHeaders: []AuthHeaderConfig{
			{Header: "x-api-key", APIKeyEnv: "X"},
		},
	}
	err := Validate(c)
	require.Error(t, err)
	require.Contains(t, err.Error(), "duplicate header")
}

// TestLoad_MimedetectOverrides_RejectsBadValue — value that doesn't
// look like a MIME (no slash, contains parameters, etc.) MUST fail.
func TestLoad_MimedetectOverrides_RejectsBadValue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "v.yaml")
	yaml := validYAML + `
mimedetect:
  extension_overrides:
    ".msg": "not-a-mime"
`
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o600))

	t.Setenv("DOCLING_API_KEY", "a")
	t.Setenv("KREUZBERG_API_KEY", "b")

	_, err := Load(path)
	require.Error(t, err)
	require.Contains(t, err.Error(), "extension_overrides")
}

const validYAML = `
server:
  listen: "0.0.0.0:8080"

routing:
  default_engine: "main"
  facade:
    docling:
      enabled: true
      prefix: "/v1"
    external:
      enabled: false
      path: "/process"
  passthrough:
    enabled: true

engines:
  main:
    enable: true
    compat_type: "docling"
    url: "http://docling.local:5001"
    api_key_env: "DOCLING_API_KEY"
    mime_types: ["application/pdf"]
  fallback:
    enable: true
    compat_type: "docling"
    url: "http://kreuzberg.local:8000"
    api_key_env: "KREUZBERG_API_KEY"
    auth_header: "Authorization"
    auth_scheme: "bearer"

security:
  proxy_api_keys_env: "PROXY_API_KEYS"
  proxy_api_key_header: "X-Proxy-Api-Key"
  require_api_key: false
`
