package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
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
	require.Equal(t, "raw", main.AuthScheme)

	kb := c.Engines["fallback"]
	require.Equal(t, CompatDocling, kb.CompatType)
	require.Equal(t, Secret("kreuzberg-secret"), kb.APIKey)
	require.Equal(t, "Authorization", kb.AuthHeader)
	require.Equal(t, "bearer", kb.AuthScheme)

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

func TestAcceptedFacades(t *testing.T) {
	t.Parallel()
	require.ElementsMatch(t, []string{"docling"}, AcceptedFacades(CompatDocling))
	require.ElementsMatch(t, []string{"external"}, AcceptedFacades(CompatExternal))
	require.ElementsMatch(t, []string{"docling", "external"}, AcceptedFacades(CompatDoclingExternal))
	require.ElementsMatch(t, []string{"docling", "external"}, AcceptedFacades(CompatTika))
	require.Empty(t, AcceptedFacades("unknown"))
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
