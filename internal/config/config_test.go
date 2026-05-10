package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSecret_RedactsInJSON(t *testing.T) {
	t.Parallel()
	// L10: a Secret value MUST never appear in JSON output. Empty
	// secrets serialise to "" so consumers can still tell unset
	// from set; non-empty secrets serialise to "***".
	type wrapper struct {
		Token Secret `json:"token"`
	}

	out, err := json.Marshal(wrapper{Token: "real-secret-value"})
	require.NoError(t, err)
	require.JSONEq(t, `{"token":"***"}`, string(out))

	out, err = json.Marshal(wrapper{Token: ""})
	require.NoError(t, err)
	require.JSONEq(t, `{"token":""}`, string(out))
}

func TestSecret_StringerRedacts(t *testing.T) {
	t.Parallel()
	require.Equal(t, "***", Secret("hunter2").String())
	require.Equal(t, "", Secret("").String())
}

func TestDefaults(t *testing.T) {
	t.Parallel()
	c := Default()
	require.Equal(t, "0.0.0.0:8080", c.Server.Listen)
	require.Equal(t, "docling", c.Routing.DefaultCEEEngine)
	require.Equal(t, "/v1", c.Routing.FacadePathPrefix)
	require.Equal(t, 30*time.Second, c.Server.ReadTimeout)
	require.True(t, c.Observability.Metrics.Enabled)
}

func TestLoad_ValidYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "valid.yaml")
	require.NoError(t, os.WriteFile(path, []byte(validYAML), 0o600))

	t.Setenv("DOCLING_API_KEY", "docling-secret")
	t.Setenv("PROXY_API_KEYS", "k1, k2 ,k3")

	c, err := Load(path)
	require.NoError(t, err)
	require.Equal(t, "tika", c.Routing.DefaultCEEEngine)
	require.True(t, c.Engines.Tika.Enable)
	require.Equal(t, "http://tika.local:9998", c.Engines.Tika.URL)
	require.Equal(t, Secret("docling-secret"), c.Engines.Docling.APIKey, "API key resolved from env")
	require.ElementsMatch(t, []Secret{"k1", "k2", "k3"}, c.Security.ProxyAPIKeys)
}

func TestLoad_EnvOverridesYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "v.yaml")
	require.NoError(t, os.WriteFile(path, []byte(validYAML), 0o600))

	t.Setenv("OWUI_PROXY_SERVER__LISTEN", "127.0.0.1:9999")
	t.Setenv("OWUI_PROXY_ENGINES__TIKA__URL", "http://override:9998")

	c, err := Load(path)
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1:9999", c.Server.Listen)
	require.Equal(t, "http://override:9998", c.Engines.Tika.URL)
}

func TestValidate_DefaultEngineMustBeEnabled(t *testing.T) {
	t.Parallel()
	c := Default()
	c.Routing.DefaultCEEEngine = "tika"
	c.Engines.Tika.Enable = false
	require.Error(t, Validate(c))
}

func TestValidate_TasksRequireRedis(t *testing.T) {
	t.Parallel()
	c := Default()
	c.Engines.Docling.Enable = true
	c.Engines.Docling.URL = "http://docling:5001"
	c.Tasks.Enabled = true
	c.Tasks.RedisURL = ""
	require.Error(t, Validate(c))
}

func TestValidate_TLSRequiresCertAndKey(t *testing.T) {
	t.Parallel()
	c := Default()
	c.Engines.Docling.Enable = true
	c.Engines.Docling.URL = "http://docling:5001"
	c.Server.TLS.Enabled = true
	require.Error(t, Validate(c))
}

const validYAML = `
server:
  listen: "0.0.0.0:8080"
  read_timeout: 30s

routing:
  default_cee_engine: "tika"
  facade_path_prefix: "/v1"

engines:
  docling:
    enable: true
    url: "http://docling.local:5001"
    api_key_env: "DOCLING_API_KEY"
  tika:
    enable: true
    url: "http://tika.local:9998"
  kreuzberg:
    enable: false

security:
  proxy_api_keys_env: "PROXY_API_KEYS"
  proxy_api_key_header: "X-Proxy-Api-Key"
  require_api_key: false
`
