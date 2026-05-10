// Package config loads and validates the proxy configuration.
//
// The loader merges three sources (lowest to highest precedence):
//  1. Built-in defaults (Default()).
//  2. YAML file at the path passed to Load.
//  3. Environment variables prefixed with OWUI_PROXY_ using "__" as
//     the path separator (e.g. OWUI_PROXY_ENGINES__TIKA__URL).
//
// Secrets (API keys) are never read from YAML. The YAML names the env
// variable (api_key_env) and the loader resolves it into the runtime
// struct via ResolveSecrets.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/go-playground/validator/v10"
	"github.com/knadh/koanf/parsers/yaml"
	envprovider "github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

// Secret wraps a string value that must never be printed or marshaled
// in cleartext. It exists so accidental inclusion in logs / debug
// dumps / JSON status endpoints redacts to "***" instead of leaking the
// secret. The underlying type is `string` so that:
//   - YAML/env deserialisation still works (koanf assigns plain strings),
//   - existing call sites can convert with `string(s)` when they need
//     the actual value (e.g. setting an Authorization header).
type Secret string

// String implements fmt.Stringer. Always returns "***" for non-empty
// secrets; empty secrets render as "" so callers can still detect
// "is the secret unset?" by comparing the Stringer output for empty —
// but the canonical empty check is `s == ""` against the typed value.
func (s Secret) String() string {
	if s == "" {
		return ""
	}
	return "***"
}

// MarshalJSON ensures Secret values never bleed into JSON-formatted
// logs or status responses. Empty secrets serialise to "" so consumers
// can distinguish "unset" from "redacted set value".
func (s Secret) MarshalJSON() ([]byte, error) {
	if s == "" {
		return json.Marshal("")
	}
	return json.Marshal("***")
}

// MarshalText mirrors MarshalJSON for text-encoders (zerolog console,
// fmt %v with stringer, etc).
func (s Secret) MarshalText() ([]byte, error) {
	return []byte(s.String()), nil
}

const (
	envPrefix     = "OWUI_PROXY_"
	envSeparator  = "__"
	keySeparator  = "."
	defaultEngine = "docling"
)

type Config struct {
	Server          ServerConfig        `koanf:"server" validate:"required"`
	Routing         RoutingConfig       `koanf:"routing" validate:"required"`
	Engines         EnginesConfig       `koanf:"engines" validate:"required"`
	Security        SecurityConfig      `koanf:"security"`
	RateLimitGlobal RateLimitConfig     `koanf:"ratelimit_global"`
	Tasks           TasksConfig         `koanf:"tasks"`
	Observability   ObservabilityConfig `koanf:"observability"`
}

type ServerConfig struct {
	Listen            string        `koanf:"listen" validate:"required,hostname_port"`
	ReadTimeout       time.Duration `koanf:"read_timeout"`
	ReadHeaderTimeout time.Duration `koanf:"read_header_timeout"`
	WriteTimeout      time.Duration `koanf:"write_timeout"`
	IdleTimeout       time.Duration `koanf:"idle_timeout"`
	// RequestTimeout caps the per-request handler deadline applied to the
	// authenticated subtree by the Timeout middleware. Distinct from the
	// transport-level Read/Write timeouts because we want streaming
	// uploads/downloads to be governed by handler context, not the socket.
	RequestTimeout time.Duration   `koanf:"request_timeout"`
	MaxHeaderBytes int             `koanf:"max_header_bytes" validate:"gte=0"`
	MaxBodyBytes   int64           `koanf:"max_body_bytes" validate:"gte=0"`
	ShutdownGrace  time.Duration   `koanf:"shutdown_grace"`
	HTTP2          bool            `koanf:"http2"`
	H2C            bool            `koanf:"h2c"`
	TLS            TLSServerConfig `koanf:"tls"`
}

type TLSServerConfig struct {
	Enabled    bool   `koanf:"enabled"`
	CertFile   string `koanf:"cert_file"`
	KeyFile    string `koanf:"key_file"`
	MinVersion string `koanf:"min_version"`
	// ClientAuth selects mTLS behavior. "require" was intentionally
	// dropped because it accepts ANY client cert without verifying it
	// against ClientCAs — almost certainly not what operators expect.
	// Use "require-and-verify" for strict mTLS.
	ClientAuth   string `koanf:"client_auth" validate:"omitempty,oneof=none request verify require-and-verify"`
	ClientCAFile string `koanf:"client_ca_file"`
}

type RoutingConfig struct {
	DefaultCEEEngine string            `koanf:"default_cee_engine" validate:"required,oneof=docling tika kreuzberg"`
	FacadePathPrefix string            `koanf:"facade_path_prefix" validate:"required,startswith=/"`
	Passthrough      PassthroughConfig `koanf:"passthrough"`
}

type PassthroughConfig struct {
	DoclingPrefix   string `koanf:"docling_prefix" validate:"omitempty,startswith=/"`
	TikaPrefix      string `koanf:"tika_prefix" validate:"omitempty,startswith=/"`
	KreuzbergPrefix string `koanf:"kreuzberg_prefix" validate:"omitempty,startswith=/"`
}

type EnginesConfig struct {
	Docling   EngineConfig `koanf:"docling"`
	Tika      EngineConfig `koanf:"tika"`
	Kreuzberg EngineConfig `koanf:"kreuzberg"`
}

type EngineConfig struct {
	Enable bool `koanf:"enable"`
	// L2: required_if uses validator's cross-field check so an enabled
	// engine must declare a URL. omitempty + url keeps the validation
	// permissive when the engine is disabled.
	URL            string        `koanf:"url" validate:"required_if=Enable true,omitempty,url"`
	APIKeyEnv      string        `koanf:"api_key_env"`
	APIKey         Secret        `koanf:"-"` // populated from env at startup
	RequestTimeout time.Duration `koanf:"request_timeout"`
	HealthPath     string        `koanf:"health_path"`
	// MimeTypes lists the MIME types this engine claims. When a request
	// is dispatched through the Docling-compatible facade, the first
	// file's Content-Type is matched against the MimeTypes lists of
	// every NON-DEFAULT enabled engine; the first match wins. Default
	// engine handles anything that does not match. Patterns are exact
	// (e.g. "application/pdf") or top-level wildcards ("image/*").
	MimeTypes      []string          `koanf:"mime_types"`
	ForwardOptions map[string]string `koanf:"forward_options"`
	HTTP           HTTPClientConfig  `koanf:"http"`
	Breaker        BreakerConfig     `koanf:"breaker"`
	RateLimit      RateLimitConfig   `koanf:"rate_limit"`
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
	ProxyAPIKeys      []Secret   `koanf:"-"` // resolved from env
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
	Enabled          bool          `koanf:"enabled"`
	RedisURLEnv      string        `koanf:"redis_url_env"`
	RedisURL         Secret        `koanf:"-"` // resolved from env
	QueueConcurrency int           `koanf:"queue_concurrency"`
	Retention        time.Duration `koanf:"retention"`
	Retry            int           `koanf:"retry"`
	ResultTTL        time.Duration `koanf:"result_ttl"`
	// TaskTimeout caps a single task's execution wall time. Distinct
	// from Retention, which is the lifetime of task state in Redis.
	TaskTimeout time.Duration `koanf:"task_timeout"`
	// MaxBlobBytes is the per-blob upper bound enforced at enqueue.
	MaxBlobBytes int64 `koanf:"max_blob_bytes" validate:"gte=0"`
	// MaxTotalBytes is the aggregate cap across all blobs in a task.
	MaxTotalBytes int64 `koanf:"max_total_bytes" validate:"gte=0"`
	// QueueWeights controls asynq's per-queue weighting; defaults to
	// {default:6, low:3, critical:1}.
	QueueWeights map[string]int `koanf:"queue_weights"`
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
			// WriteTimeout previously defaulted to 0 (unbounded), letting
			// slow-loris-style response stalls hold sockets indefinitely.
			// 5m matches RequestTimeout: a single request that legitimately
			// takes that long will not be cut off mid-stream.
			WriteTimeout: 5 * time.Minute,
			IdleTimeout:  120 * time.Second,
			// RequestTimeout governs the handler-level deadline applied
			// by the Timeout middleware on the authenticated subtree.
			RequestTimeout: 5 * time.Minute,
			MaxHeaderBytes: 1 << 20,
			MaxBodyBytes:   500 << 20,
			ShutdownGrace:  30 * time.Second,
			HTTP2:          true,
			TLS: TLSServerConfig{
				MinVersion: "1.3",
				ClientAuth: "none",
			},
		},
		Routing: RoutingConfig{
			DefaultCEEEngine: defaultEngine,
			FacadePathPrefix: "/v1",
			Passthrough: PassthroughConfig{
				DoclingPrefix:   "/docling",
				TikaPrefix:      "/tika",
				KreuzbergPrefix: "/kreuzberg",
			},
		},
		Engines: EnginesConfig{
			Docling:   defaultEngineConfig(),
			Tika:      defaultEngineConfig(),
			Kreuzberg: defaultEngineConfig(),
		},
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
			MaxBlobBytes:     100 << 20, // 100 MiB
			MaxTotalBytes:    500 << 20, // 500 MiB
			QueueWeights:     map[string]int{"default": 6, "low": 3, "critical": 1},
		},
		Observability: ObservabilityConfig{
			// AddCaller defaults to false: caller resolution is moderately
			// expensive (per-line runtime.Caller) and operators should opt
			// in only for debug investigations.
			Log:     LogConfig{Level: "info", Format: "json", AddCaller: false},
			Metrics: MetricsConfig{Enabled: true, Path: "/metrics"},
			Tracing: TracingConfig{Enabled: false, SampleRatio: 0.1, ServiceName: "owui-cee-proxy"},
		},
	}
}

func defaultEngineConfig() EngineConfig {
	return EngineConfig{
		Enable:         false,
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
	// M9: koanf merges env over YAML by overwriting scalar keys, but
	// SLICE-typed YAML keys (e.g. engines.tika.mime_types,
	// security.trusted_proxies, security.proxy_api_keys, tasks
	// queue_weights map) cannot be appended to via env vars — there is
	// no list-append semantics in the env provider. Operators wanting
	// to override a slice MUST replace the entire YAML key, e.g. by
	// shipping a new YAML file or by setting the JSON-encoded value
	// through whatever runtime they use to launch the proxy. The
	// scalar-only keys (URL, paths, header names, etc.) compose
	// normally with OWUI_PROXY_* env vars.
	if err := k.Load(envprovider.Provider(envPrefix, keySeparator, envTransform), nil); err != nil {
		return nil, fmt.Errorf("load env: %w", err)
	}
	out := &Config{}
	if err := k.UnmarshalWithConf("", out, koanf.UnmarshalConf{Tag: "koanf"}); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
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

func resolveSecrets(c *Config) {
	resolveEngineSecret(&c.Engines.Docling)
	resolveEngineSecret(&c.Engines.Tika)
	resolveEngineSecret(&c.Engines.Kreuzberg)
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

func resolveEngineSecret(e *EngineConfig) {
	if e.APIKeyEnv == "" {
		return
	}
	e.APIKey = Secret(os.Getenv(e.APIKeyEnv))
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
	if err := validateEngineSelected(c); err != nil {
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

func validateEngineSelected(c *Config) error {
	switch c.Routing.DefaultCEEEngine {
	case "docling":
		if !c.Engines.Docling.Enable {
			return fmt.Errorf("default_cee_engine=docling but engines.docling.enable=false")
		}
		if c.Engines.Docling.URL == "" {
			return fmt.Errorf("engines.docling.url is required when enabled")
		}
	case "tika":
		if !c.Engines.Tika.Enable {
			return fmt.Errorf("default_cee_engine=tika but engines.tika.enable=false")
		}
		if c.Engines.Tika.URL == "" {
			return fmt.Errorf("engines.tika.url is required when enabled")
		}
	case "kreuzberg":
		if !c.Engines.Kreuzberg.Enable {
			return fmt.Errorf("default_cee_engine=kreuzberg but engines.kreuzberg.enable=false")
		}
		if c.Engines.Kreuzberg.URL == "" {
			return fmt.Errorf("engines.kreuzberg.url is required when enabled")
		}
	}
	return nil
}
