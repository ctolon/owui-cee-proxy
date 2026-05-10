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

const (
	envPrefix     = "OWUI_PROXY_"
	envSeparator  = "__"
	keySeparator  = "."
	defaultEngine = "docling"
)

type Config struct {
	Server          ServerConfig          `koanf:"server" validate:"required"`
	Routing         RoutingConfig         `koanf:"routing" validate:"required"`
	Engines         EnginesConfig         `koanf:"engines" validate:"required"`
	Security        SecurityConfig        `koanf:"security"`
	RateLimitGlobal RateLimitConfig       `koanf:"ratelimit_global"`
	Tasks           TasksConfig           `koanf:"tasks"`
	Observability   ObservabilityConfig   `koanf:"observability"`
}

type ServerConfig struct {
	Listen            string        `koanf:"listen" validate:"required,hostname_port"`
	ReadTimeout       time.Duration `koanf:"read_timeout"`
	ReadHeaderTimeout time.Duration `koanf:"read_header_timeout"`
	WriteTimeout      time.Duration `koanf:"write_timeout"`
	IdleTimeout       time.Duration `koanf:"idle_timeout"`
	MaxHeaderBytes    int           `koanf:"max_header_bytes" validate:"gte=0"`
	MaxBodyBytes      int64         `koanf:"max_body_bytes" validate:"gte=0"`
	ShutdownGrace     time.Duration `koanf:"shutdown_grace"`
	HTTP2             bool          `koanf:"http2"`
	H2C               bool          `koanf:"h2c"`
	TLS               TLSServerConfig `koanf:"tls"`
}

type TLSServerConfig struct {
	Enabled       bool   `koanf:"enabled"`
	CertFile      string `koanf:"cert_file"`
	KeyFile       string `koanf:"key_file"`
	MinVersion    string `koanf:"min_version"`
	ClientAuth    string `koanf:"client_auth" validate:"omitempty,oneof=none request require verify require-and-verify"`
	ClientCAFile  string `koanf:"client_ca_file"`
}

type RoutingConfig struct {
	DefaultCEEEngine    string `koanf:"default_cee_engine" validate:"required,oneof=docling tika kreuzberg"`
	FacadePathPrefix    string `koanf:"facade_path_prefix" validate:"required,startswith=/"`
	Passthrough         PassthroughConfig `koanf:"passthrough"`
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
	Enable          bool              `koanf:"enable"`
	URL             string            `koanf:"url" validate:"omitempty,url"`
	APIKeyEnv       string            `koanf:"api_key_env"`
	APIKey          string            `koanf:"-"` // populated from env at startup
	RequestTimeout  time.Duration     `koanf:"request_timeout"`
	HealthPath      string            `koanf:"health_path"`
	// MimeTypes lists the MIME types this engine claims. When a request
	// is dispatched through the Docling-compatible facade, the first
	// file's Content-Type is matched against the MimeTypes lists of
	// every NON-DEFAULT enabled engine; the first match wins. Default
	// engine handles anything that does not match. Patterns are exact
	// (e.g. "application/pdf") or top-level wildcards ("image/*").
	MimeTypes       []string          `koanf:"mime_types"`
	ForwardOptions  map[string]string `koanf:"forward_options"`
	HTTP            HTTPClientConfig  `koanf:"http"`
	Breaker         BreakerConfig     `koanf:"breaker"`
	RateLimit       RateLimitConfig   `koanf:"rate_limit"`
}

type HTTPClientConfig struct {
	MaxIdleConns          int           `koanf:"max_idle_conns"`
	MaxIdleConnsPerHost   int           `koanf:"max_idle_conns_per_host"`
	MaxConnsPerHost       int           `koanf:"max_conns_per_host"`
	IdleConnTimeout       time.Duration `koanf:"idle_conn_timeout"`
	TLSHandshakeTimeout   time.Duration `koanf:"tls_handshake_timeout"`
	ResponseHeaderTimeout time.Duration `koanf:"response_header_timeout"`
	ExpectContinueTimeout time.Duration `koanf:"expect_continue_timeout"`
	DisableCompression    bool          `koanf:"disable_compression"`
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
	ProxyAPIKeysEnv   string   `koanf:"proxy_api_keys_env"`
	ProxyAPIKeyHeader string   `koanf:"proxy_api_key_header"`
	ProxyAPIKeys      []string `koanf:"-"` // resolved from env
	RequireAPIKey     bool     `koanf:"require_api_key"`
	TrustedProxies    []string `koanf:"trusted_proxies"`
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
	RedisURL         string        `koanf:"-"` // resolved from env
	QueueConcurrency int           `koanf:"queue_concurrency"`
	Retention        time.Duration `koanf:"retention"`
	Retry            int           `koanf:"retry"`
	ResultTTL        time.Duration `koanf:"result_ttl"`
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
			WriteTimeout:      0,
			IdleTimeout:       120 * time.Second,
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
		},
		Observability: ObservabilityConfig{
			Log:     LogConfig{Level: "info", Format: "json", AddCaller: true},
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
			c.Security.ProxyAPIKeys = splitAndTrim(v)
		}
	}
	if c.Tasks.RedisURLEnv != "" {
		c.Tasks.RedisURL = os.Getenv(c.Tasks.RedisURLEnv)
	}
}

func resolveEngineSecret(e *EngineConfig) {
	if e.APIKeyEnv == "" {
		return
	}
	e.APIKey = os.Getenv(e.APIKeyEnv)
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
