// Package config loads and validates the proxy configuration.
//
// The loader merges three sources (lowest to highest precedence):
//  1. Built-in defaults (Default()).
//  2. YAML file at the path passed to Load.
//  3. Environment variables prefixed with OWUI_PROXY_ using "__" as
//     the path separator (e.g. OWUI_PROXY_ENGINES__MAIN-DOCLING__URL).
//
// Secrets (API keys) are never read from YAML. The YAML names the env
// variable (api_key_env) and the loader resolves it into the runtime
// struct via resolveSecrets.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/go-playground/validator/v10"
	"github.com/knadh/koanf/parsers/yaml"
	envprovider "github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

// Secret wraps a string value that must never be printed or marshaled
// in cleartext.
type Secret string

func (s Secret) String() string {
	if s == "" {
		return ""
	}
	return "***"
}

func (s Secret) MarshalJSON() ([]byte, error) {
	if s == "" {
		return json.Marshal("")
	}
	return json.Marshal("***")
}

func (s Secret) MarshalText() ([]byte, error) {
	return []byte(s.String()), nil
}

const (
	envPrefix    = "OWUI_PROXY_"
	envSeparator = "__"
	keySeparator = "."
)

// CompatType identifies the wire-protocol family an engine speaks. The
// transport layer dispatches each request to the adapter package
// matching this value.
const (
	CompatDocling         = "docling"
	CompatExternal        = "external"
	CompatDoclingExternal = "docling-external"
	CompatTika            = "tika"
)

// Engine name validation. We use the YAML key as a URL path segment
// for the passthrough mounts; restrict it to a safe charset.
var engineNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// Auth header validation: visible ASCII only, no CR/LF, reasonable
// length cap.
var authHeaderRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]{0,63}$`)

type Config struct {
	Server          ServerConfig            `koanf:"server" validate:"required"`
	Routing         RoutingConfig           `koanf:"routing" validate:"required"`
	Engines         map[string]EngineConfig `koanf:"engines" validate:"required,dive"`
	Security        SecurityConfig          `koanf:"security"`
	RateLimitGlobal RateLimitConfig         `koanf:"ratelimit_global"`
	Tasks           TasksConfig             `koanf:"tasks"`
	Observability   ObservabilityConfig     `koanf:"observability"`
}

type ServerConfig struct {
	Listen            string          `koanf:"listen" validate:"required,hostname_port"`
	ReadTimeout       time.Duration   `koanf:"read_timeout"`
	ReadHeaderTimeout time.Duration   `koanf:"read_header_timeout"`
	WriteTimeout      time.Duration   `koanf:"write_timeout"`
	IdleTimeout       time.Duration   `koanf:"idle_timeout"`
	RequestTimeout    time.Duration   `koanf:"request_timeout"`
	MaxHeaderBytes    int             `koanf:"max_header_bytes" validate:"gte=0"`
	MaxBodyBytes      int64           `koanf:"max_body_bytes" validate:"gte=0"`
	ShutdownGrace     time.Duration   `koanf:"shutdown_grace"`
	HTTP2             bool            `koanf:"http2"`
	H2C               bool            `koanf:"h2c"`
	TLS               TLSServerConfig `koanf:"tls"`
}

type TLSServerConfig struct {
	Enabled      bool   `koanf:"enabled"`
	CertFile     string `koanf:"cert_file"`
	KeyFile      string `koanf:"key_file"`
	MinVersion   string `koanf:"min_version"`
	ClientAuth   string `koanf:"client_auth" validate:"omitempty,oneof=none request verify require-and-verify"`
	ClientCAFile string `koanf:"client_ca_file"`
}

type RoutingConfig struct {
	// DefaultEngine references a key in Engines. The named engine MUST
	// be enabled; cross-field validation in Validate() enforces this.
	DefaultEngine string            `koanf:"default_engine" validate:"required"`
	Facade        FacadeConfig      `koanf:"facade"`
	Passthrough   PassthroughConfig `koanf:"passthrough"`
}

// FacadeConfig controls which client-facing protocols the proxy
// exposes.
type FacadeConfig struct {
	Docling  DoclingFacadeConfig  `koanf:"docling"`
	External ExternalFacadeConfig `koanf:"external"`
}

type DoclingFacadeConfig struct {
	Enabled bool   `koanf:"enabled"`
	Prefix  string `koanf:"prefix" validate:"omitempty,startswith=/"`
}

type ExternalFacadeConfig struct {
	Enabled bool `koanf:"enabled"`
	// Path is the route mounted to accept PUT raw-body requests, per
	// OpenWebUI's external loader contract (default `/process`).
	Path string `koanf:"path" validate:"omitempty,startswith=/"`
}

type PassthroughConfig struct {
	Enabled bool `koanf:"enabled"`
}

// EngineConfig is the per-engine entry under the engines map. The map
// key (e.g., "main-docling") is the engine's name; this struct holds
// the rest.
type EngineConfig struct {
	Enable bool `koanf:"enable"`
	// CompatType selects the adapter wire-protocol family.
	CompatType string `koanf:"compat_type" validate:"required_if=Enable true,omitempty,oneof=docling external docling-external tika"`
	URL        string `koanf:"url" validate:"required_if=Enable true,omitempty,url"`
	APIKeyEnv  string `koanf:"api_key_env"`
	APIKey     Secret `koanf:"-"`
	// AuthHeader is the outbound HTTP header name to stamp with the
	// engine's API key. Defaults supplied per compat_type by Validate
	// when empty.
	AuthHeader string `koanf:"auth_header" validate:"omitempty"`
	// AuthScheme adjusts how the API key is rendered. "raw" uses the
	// key value as-is (e.g., "X-Api-Key: abc123"); "bearer" prepends
	// "Bearer " (e.g., "Authorization: Bearer abc123").
	AuthScheme     string            `koanf:"auth_scheme" validate:"omitempty,oneof=raw bearer"`
	RequestTimeout time.Duration     `koanf:"request_timeout"`
	HealthPath     string            `koanf:"health_path"`
	MimeTypes      []string          `koanf:"mime_types"`
	Paths          EnginePathsConfig `koanf:"paths"`
	ForwardOptions map[string]string `koanf:"forward_options"`
	HTTP           HTTPClientConfig  `koanf:"http"`
	Breaker        BreakerConfig     `koanf:"breaker"`
	RateLimit      RateLimitConfig   `koanf:"rate_limit"`
}

// EnginePathsConfig overrides per-compat default paths. When a field
// is empty the adapter uses its built-in default.
type EnginePathsConfig struct {
	// docling-family
	DoclingConvertFile   string `koanf:"docling_convert_file"`
	DoclingConvertSource string `koanf:"docling_convert_source"`
	// external-family
	ExternalProcess string `koanf:"external_process"`
	// tika-family
	TikaText string `koanf:"tika_text"`
}

type HTTPClientConfig struct {
	MaxIdleConns          int             `koanf:"max_idle_conns"`
	MaxIdleConnsPerHost   int             `koanf:"max_idle_conns_per_host"`
	MaxConnsPerHost       int             `koanf:"max_conns_per_host"`
	IdleConnTimeout       time.Duration   `koanf:"idle_conn_timeout"`
	TLSHandshakeTimeout   time.Duration   `koanf:"tls_handshake_timeout"`
	ResponseHeaderTimeout time.Duration   `koanf:"response_header_timeout"`
	ExpectContinueTimeout time.Duration   `koanf:"expect_continue_timeout"`
	DisableCompression    bool            `koanf:"disable_compression"`
	TLS                   TLSClientConfig `koanf:"tls"`
}

type TLSClientConfig struct {
	InsecureSkipVerify bool   `koanf:"insecure_skip_verify"`
	CAFile             string `koanf:"ca_file"`
	CertFile           string `koanf:"cert_file"`
	KeyFile            string `koanf:"key_file"`
	ServerName         string `koanf:"server_name"`
}

type BreakerConfig struct {
	MaxRequests                  uint32        `koanf:"max_requests"`
	Interval                     time.Duration `koanf:"interval"`
	Timeout                      time.Duration `koanf:"timeout"`
	ConsecutiveFailuresThreshold uint32        `koanf:"consecutive_failures_threshold"`
}

type RateLimitConfig struct {
	RPS   float64 `koanf:"rps"`
	Burst int     `koanf:"burst"`
}

type SecurityConfig struct {
	ProxyAPIKeysEnv   string     `koanf:"proxy_api_keys_env"`
	ProxyAPIKeyHeader string     `koanf:"proxy_api_key_header"`
	ProxyAPIKeys      []Secret   `koanf:"-"`
	RequireAPIKey     bool       `koanf:"require_api_key"`
	TrustedProxies    []string   `koanf:"trusted_proxies"`
	CORS              CORSConfig `koanf:"cors"`
}

type CORSConfig struct {
	Enabled        bool     `koanf:"enabled"`
	AllowedOrigins []string `koanf:"allowed_origins"`
	AllowedMethods []string `koanf:"allowed_methods"`
	AllowedHeaders []string `koanf:"allowed_headers"`
	MaxAge         int      `koanf:"max_age"`
}

type TasksConfig struct {
	Enabled          bool           `koanf:"enabled"`
	RedisURLEnv      string         `koanf:"redis_url_env"`
	RedisURL         Secret         `koanf:"-"`
	QueueConcurrency int            `koanf:"queue_concurrency"`
	Retention        time.Duration  `koanf:"retention"`
	Retry            int            `koanf:"retry"`
	ResultTTL        time.Duration  `koanf:"result_ttl"`
	TaskTimeout      time.Duration  `koanf:"task_timeout"`
	MaxBlobBytes     int64          `koanf:"max_blob_bytes" validate:"gte=0"`
	MaxTotalBytes    int64          `koanf:"max_total_bytes" validate:"gte=0"`
	QueueWeights     map[string]int `koanf:"queue_weights"`
}

type ObservabilityConfig struct {
	Log     LogConfig     `koanf:"log"`
	Metrics MetricsConfig `koanf:"metrics"`
	Tracing TracingConfig `koanf:"tracing"`
}

type LogConfig struct {
	Level     string `koanf:"level" validate:"omitempty,oneof=trace debug info warn error fatal panic"`
	Format    string `koanf:"format" validate:"omitempty,oneof=json console"`
	Sampling  bool   `koanf:"sampling"`
	AddCaller bool   `koanf:"add_caller"`
}

type MetricsConfig struct {
	Enabled bool   `koanf:"enabled"`
	Path    string `koanf:"path"`
	Listen  string `koanf:"listen"`
}

type TracingConfig struct {
	Enabled      bool    `koanf:"enabled"`
	OTLPEndpoint string  `koanf:"otlp_endpoint"`
	SampleRatio  float64 `koanf:"sample_ratio" validate:"gte=0,lte=1"`
	ServiceName  string  `koanf:"service_name"`
}

// Default returns a baseline configuration. Used as the lowest-precedence
// merge layer.
func Default() *Config {
	return &Config{
		Server: ServerConfig{
			Listen:            "0.0.0.0:8080",
			ReadTimeout:       30 * time.Second,
			ReadHeaderTimeout: 10 * time.Second,
			WriteTimeout:      5 * time.Minute,
			IdleTimeout:       120 * time.Second,
			RequestTimeout:    5 * time.Minute,
			MaxHeaderBytes:    1 << 20,
			MaxBodyBytes:      500 << 20,
			ShutdownGrace:     30 * time.Second,
			HTTP2:             true,
			TLS: TLSServerConfig{
				MinVersion: "1.3",
				ClientAuth: "none",
			},
		},
		Routing: RoutingConfig{
			DefaultEngine: "",
			Facade: FacadeConfig{
				Docling:  DoclingFacadeConfig{Enabled: true, Prefix: "/v1"},
				External: ExternalFacadeConfig{Enabled: true, Path: "/process"},
			},
			Passthrough: PassthroughConfig{Enabled: true},
		},
		Engines: map[string]EngineConfig{},
		Security: SecurityConfig{
			ProxyAPIKeysEnv:   "PROXY_API_KEYS",
			ProxyAPIKeyHeader: "X-Proxy-Api-Key",
			RequireAPIKey:     false,
		},
		RateLimitGlobal: RateLimitConfig{RPS: 200, Burst: 400},
		Tasks: TasksConfig{
			Enabled:          false,
			RedisURLEnv:      "REDIS_URL",
			QueueConcurrency: 16,
			Retention:        24 * time.Hour,
			Retry:            3,
			ResultTTL:        time.Hour,
			TaskTimeout:      5 * time.Minute,
			MaxBlobBytes:     100 << 20,
			MaxTotalBytes:    500 << 20,
			QueueWeights:     map[string]int{"default": 6, "low": 3, "critical": 1},
		},
		Observability: ObservabilityConfig{
			Log:     LogConfig{Level: "info", Format: "json", AddCaller: false},
			Metrics: MetricsConfig{Enabled: true, Path: "/metrics"},
			Tracing: TracingConfig{Enabled: false, SampleRatio: 0.1, ServiceName: "owui-cee-proxy"},
		},
	}
}

// DefaultEngineConfig returns a baseline EngineConfig used as a
// per-entry merge layer. Exported so the migration helper can reuse it.
func DefaultEngineConfig() EngineConfig {
	return EngineConfig{
		Enable:         false,
		AuthScheme:     "raw",
		RequestTimeout: 120 * time.Second,
		HealthPath:     "/health",
		HTTP: HTTPClientConfig{
			MaxIdleConns:          256,
			MaxIdleConnsPerHost:   64,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			ExpectContinueTimeout: time.Second,
		},
		Breaker: BreakerConfig{
			MaxRequests:                  100,
			Interval:                     60 * time.Second,
			Timeout:                      30 * time.Second,
			ConsecutiveFailuresThreshold: 5,
		},
		RateLimit: RateLimitConfig{RPS: 50, Burst: 100},
	}
}

// Load reads YAML from path (if non-empty), overlays env, validates,
// resolves secrets, and returns the final Config.
func Load(path string) (*Config, error) {
	k := koanf.New(keySeparator)
	cfg := Default()
	if err := k.Load(structsLoader(cfg), nil); err != nil {
		return nil, fmt.Errorf("load defaults: %w", err)
	}
	if path != "" {
		if err := k.Load(file.Provider(path), yaml.Parser()); err != nil {
			return nil, fmt.Errorf("load yaml %q: %w", path, err)
		}
	}
	// koanf merges env over YAML by overwriting scalar keys. SLICE/map
	// YAML keys (engines.<n>.mime_types, security.trusted_proxies,
	// security.proxy_api_keys, tasks.queue_weights, the engines map
	// itself) cannot be appended to via env vars. Operators wanting to
	// override a slice/map MUST replace the entire YAML key.
	if err := k.Load(envprovider.Provider(envPrefix, keySeparator, envTransform), nil); err != nil {
		return nil, fmt.Errorf("load env: %w", err)
	}
	out := &Config{}
	if err := k.UnmarshalWithConf("", out, koanf.UnmarshalConf{Tag: "koanf"}); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	applyEngineDefaults(out)
	resolveSecrets(out)
	if err := Validate(out); err != nil {
		return nil, err
	}
	return out, nil
}

// envTransform converts OWUI_PROXY_FOO__BAR_BAZ → foo.bar_baz
func envTransform(s string) string {
	s = strings.TrimPrefix(s, envPrefix)
	s = strings.ToLower(s)
	return strings.ReplaceAll(s, envSeparator, keySeparator)
}

// applyEngineDefaults fills missing per-engine fields from
// DefaultEngineConfig() so callers can write a concise YAML stanza
// without repeating http/breaker/rate_limit subtrees.
func applyEngineDefaults(c *Config) {
	def := DefaultEngineConfig()
	for name, ec := range c.Engines {
		if ec.AuthScheme == "" {
			ec.AuthScheme = def.AuthScheme
		}
		if ec.RequestTimeout == 0 {
			ec.RequestTimeout = def.RequestTimeout
		}
		if ec.HealthPath == "" {
			ec.HealthPath = def.HealthPath
		}
		if ec.HTTP.MaxIdleConns == 0 {
			ec.HTTP.MaxIdleConns = def.HTTP.MaxIdleConns
		}
		if ec.HTTP.MaxIdleConnsPerHost == 0 {
			ec.HTTP.MaxIdleConnsPerHost = def.HTTP.MaxIdleConnsPerHost
		}
		if ec.HTTP.IdleConnTimeout == 0 {
			ec.HTTP.IdleConnTimeout = def.HTTP.IdleConnTimeout
		}
		if ec.HTTP.TLSHandshakeTimeout == 0 {
			ec.HTTP.TLSHandshakeTimeout = def.HTTP.TLSHandshakeTimeout
		}
		if ec.HTTP.ResponseHeaderTimeout == 0 {
			ec.HTTP.ResponseHeaderTimeout = def.HTTP.ResponseHeaderTimeout
		}
		if ec.HTTP.ExpectContinueTimeout == 0 {
			ec.HTTP.ExpectContinueTimeout = def.HTTP.ExpectContinueTimeout
		}
		if ec.Breaker.MaxRequests == 0 {
			ec.Breaker = def.Breaker
		}
		if ec.RateLimit.RPS == 0 && ec.RateLimit.Burst == 0 {
			ec.RateLimit = def.RateLimit
		}
		// Default auth header by compat_type when unset.
		if ec.AuthHeader == "" {
			switch ec.CompatType {
			case CompatExternal:
				ec.AuthHeader = "Authorization"
				if ec.AuthScheme == "raw" {
					ec.AuthScheme = "bearer"
				}
			default:
				ec.AuthHeader = "X-Api-Key"
			}
		}
		c.Engines[name] = ec
	}
}

func resolveSecrets(c *Config) {
	for name, ec := range c.Engines {
		if ec.APIKeyEnv != "" {
			ec.APIKey = Secret(os.Getenv(ec.APIKeyEnv))
		}
		c.Engines[name] = ec
	}
	if c.Security.ProxyAPIKeysEnv != "" {
		if v := os.Getenv(c.Security.ProxyAPIKeysEnv); v != "" {
			parts := splitAndTrim(v)
			out := make([]Secret, len(parts))
			for i, p := range parts {
				out[i] = Secret(p)
			}
			c.Security.ProxyAPIKeys = out
		}
	}
	if c.Tasks.RedisURLEnv != "" {
		c.Tasks.RedisURL = Secret(os.Getenv(c.Tasks.RedisURLEnv))
	}
}

func splitAndTrim(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Validate runs struct validation and cross-field rules.
func Validate(c *Config) error {
	v := validator.New(validator.WithRequiredStructEnabled())
	if err := v.Struct(c); err != nil {
		return fmt.Errorf("config validation: %w", err)
	}
	if err := validateEngineNames(c); err != nil {
		return err
	}
	if err := validateAuthHeaders(c); err != nil {
		return err
	}
	if err := validateDefaultEngine(c); err != nil {
		return err
	}
	if err := validateFacadeCoverage(c); err != nil {
		return err
	}
	if err := validateEnginePaths(c); err != nil {
		return err
	}
	if c.Tasks.Enabled && c.Tasks.RedisURL == "" {
		return fmt.Errorf("tasks.enabled=true but %s env is empty", c.Tasks.RedisURLEnv)
	}
	if c.Tasks.Enabled {
		if c.Tasks.MaxTotalBytes > 0 && c.Tasks.MaxBlobBytes > c.Tasks.MaxTotalBytes {
			return fmt.Errorf("tasks.max_blob_bytes (%d) must be <= max_total_bytes (%d)", c.Tasks.MaxBlobBytes, c.Tasks.MaxTotalBytes)
		}
		if c.Tasks.TaskTimeout < 0 {
			return fmt.Errorf("tasks.task_timeout must be non-negative")
		}
		for q, w := range c.Tasks.QueueWeights {
			if w < 0 {
				return fmt.Errorf("tasks.queue_weights[%q] must be non-negative", q)
			}
		}
	}
	if c.Server.TLS.Enabled && (c.Server.TLS.CertFile == "" || c.Server.TLS.KeyFile == "") {
		return fmt.Errorf("server.tls.enabled=true requires cert_file and key_file")
	}
	return nil
}

func validateEngineNames(c *Config) error {
	for name := range c.Engines {
		if !engineNameRE.MatchString(name) {
			return fmt.Errorf("engines.%q: invalid name; must match %s", name, engineNameRE)
		}
	}
	return nil
}

func validateAuthHeaders(c *Config) error {
	for name, ec := range c.Engines {
		if !ec.Enable {
			continue
		}
		if ec.AuthHeader != "" && !authHeaderRE.MatchString(ec.AuthHeader) {
			return fmt.Errorf("engines.%s.auth_header %q: invalid header name; must match %s", name, ec.AuthHeader, authHeaderRE)
		}
	}
	return nil
}

func validateDefaultEngine(c *Config) error {
	if c.Routing.DefaultEngine == "" {
		return fmt.Errorf("routing.default_engine is required")
	}
	ec, ok := c.Engines[c.Routing.DefaultEngine]
	if !ok {
		return fmt.Errorf("routing.default_engine=%q not found in engines", c.Routing.DefaultEngine)
	}
	if !ec.Enable {
		return fmt.Errorf("routing.default_engine=%q must be enabled", c.Routing.DefaultEngine)
	}
	if ec.URL == "" {
		return fmt.Errorf("engines.%s.url is required when enabled", c.Routing.DefaultEngine)
	}
	return nil
}

// validateFacadeCoverage ensures that every enabled facade has at
// least one enabled engine that answers it. AcceptedFacades returns
// the set of facades a compat_type can answer.
func validateFacadeCoverage(c *Config) error {
	doclingCount, externalCount := 0, 0
	for _, ec := range c.Engines {
		if !ec.Enable {
			continue
		}
		facades := acceptedFacades(ec.CompatType)
		for _, f := range facades {
			switch f {
			case "docling":
				doclingCount++
			case "external":
				externalCount++
			}
		}
	}
	if c.Routing.Facade.Docling.Enabled && doclingCount == 0 {
		return fmt.Errorf("routing.facade.docling.enabled=true but no engine answers the docling facade")
	}
	if c.Routing.Facade.External.Enabled && externalCount == 0 {
		return fmt.Errorf("routing.facade.external.enabled=true but no engine answers the external facade")
	}
	return nil
}

// acceptedFacades is the canonical mapping from compat_type to the
// facades an engine of that compat_type answers. This is the single
// source of truth used by config validation; the engine adapters'
// Capabilities() method reads the same logic via the same lookup.
func acceptedFacades(compat string) []string {
	switch compat {
	case CompatDocling:
		return []string{"docling"}
	case CompatExternal:
		return []string{"external"}
	case CompatDoclingExternal:
		return []string{"docling", "external"}
	case CompatTika:
		// Tika's wire format is sui generis but the proxy adapts it for
		// either inbound facade.
		return []string{"docling", "external"}
	default:
		return nil
	}
}

// AcceptedFacades is an exported wrapper used by the engine adapters
// so the canonical compat_type → facade mapping lives in this package.
func AcceptedFacades(compat string) []string { return acceptedFacades(compat) }

// validateEnginePaths enforces that docling-external engines declare
// both endpoint paths (or rely on built-in defaults — declared empty
// means use the adapter default).
func validateEnginePaths(c *Config) error {
	// Currently the adapter defaults handle empty path overrides, so
	// no per-compat path enforcement is required at validation time.
	// Reserved for future use (e.g., when a compat_type lacks a
	// canonical default path).
	_ = c
	return nil
}
