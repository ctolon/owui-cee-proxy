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
	"net"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/go-playground/validator/v10"
	"github.com/knadh/koanf/parsers/yaml"
	envprovider "github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"

	"github.com/ctolon/owui-cee-proxy/internal/engine"
)

// RoutingStrategy re-exports engine.RoutingStrategy so YAML/validator
// machinery can refer to it without forcing the engine package to
// import config (CLAUDE.md layering invariant: engine never depends
// on config). The Go type alias means a config.RoutingStrategy IS an
// engine.RoutingStrategy, no conversion needed at call sites.
type RoutingStrategy = engine.RoutingStrategy

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
// matching this value. Declared as a typed string so misspelled
// switch arms are caught at compile time.
type CompatType string

const (
	CompatDocling         CompatType = "docling"
	CompatExternal        CompatType = "external"
	CompatDoclingExternal CompatType = "docling-external"
	CompatTika            CompatType = "tika"
)

// compatTypes is THE registered set of compat_type values the proxy
// recognises. The custom validator (registered in Validate) reads
// off this slice, the bootstrap log enumerates it, and
// acceptedFacades's switch is structurally coupled to it. Adding a
// new compat_type is now THREE single-line edits: one const above,
// one entry here, one acceptedFacades arm — the validator tag no
// longer needs hand-syncing.
//
// (C-26 from REVIEW-FAANG.md: previously the validator tag carried a
// duplicate `oneof=docling external docling-external tika` literal
// that drifted from the Compat* consts on every new compat type.)
var compatTypes = []CompatType{
	CompatDocling,
	CompatExternal,
	CompatDoclingExternal,
	CompatTika,
}

// CompatTypes returns a defensive copy of the registered compat_type
// set. Adapter packages and enginetest contracts consult this to
// stay in lockstep with the validator's allowed list.
func CompatTypes() []CompatType {
	out := make([]CompatType, len(compatTypes))
	copy(out, compatTypes)
	return out
}

// AuthScheme controls how an API key is rendered on outbound
// requests. Typed for the same reason as CompatType — switch arms
// against a misspelled scheme literal would otherwise silently
// degrade auth (and we just lived through that bug on the
// passthrough path).
type AuthScheme string

const (
	AuthSchemeRaw    AuthScheme = "raw"
	AuthSchemeBearer AuthScheme = "bearer"
)

// Engine name validation. We use the YAML key as a URL path segment
// for the passthrough mounts; restrict it to a safe charset.
var engineNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// Auth header validation: visible ASCII only, no CR/LF, reasonable
// length cap.
var authHeaderRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]{0,63}$`)

// Extension key for mimedetect.extension_overrides. Single leading dot,
// lowercase alphanumeric run, length-capped. We deliberately exclude
// multi-segment forms (".tar.gz") — filepath.Ext only returns the
// terminal suffix, so a multi-segment key would never match and
// would silently confuse operators.
var extensionOverrideKeyRE = regexp.MustCompile(`^\.[a-z0-9]{1,15}$`)

// MIME value for mimedetect.extension_overrides. Structural sanity:
// "type/subtype" with the printable, non-whitespace alphabets the
// IANA grammar permits. We don't enforce IANA registration — many
// real-world MIME types are de-facto (e.g.,
// application/vnd.ms-outlook). Parameters (";charset=...") are
// rejected — overrides should encode the canonical bare media type.
var mimeValueRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9!#$&^_.+-]{0,126}/[A-Za-z0-9][A-Za-z0-9!#$&^_.+-]{0,126}$`)

type Config struct {
	Server          ServerConfig            `koanf:"server" validate:"required"`
	Routing         RoutingConfig           `koanf:"routing" validate:"required"`
	Engines         map[string]EngineConfig `koanf:"engines" validate:"required,dive"`
	Security        SecurityConfig          `koanf:"security"`
	RateLimitGlobal RateLimitConfig         `koanf:"ratelimit_global"`
	Tasks           TasksConfig             `koanf:"tasks"`
	Observability   ObservabilityConfig     `koanf:"observability"`
	// Mimedetect configures the MIME resolution stage that sits in
	// front of registry dispatch — sniff thresholds, CFB-ambiguity
	// overrides, etc.
	Mimedetect MimedetectConfig `koanf:"mimedetect"`
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
	DefaultEngine string `koanf:"default_engine" validate:"required"`
	// Strategy selects how the registry picks an engine for a given
	// facade + request hint. Empty value resolves to
	// `mime_then_extension` at registry construction time, which is
	// also the bootstrap default — that combination preserves v0.4.x
	// MIME-only behaviour whenever no engine declares an `extensions`
	// list. See internal/engine/routing_strategy.go for semantics.
	Strategy    RoutingStrategy   `koanf:"strategy" validate:"omitempty,oneof=mime extension mime_then_extension extension_then_mime"`
	Facade      FacadeConfig      `koanf:"facade"`
	Passthrough PassthroughConfig `koanf:"passthrough"`
	// Fallback enables the engine-fallback chain (C-31). When ON,
	// a 5xx / breaker-open from the primary engine pick triggers
	// dispatch to the next candidate in the registry's order list
	// for the same facade. Default OFF — the legacy "first match
	// wins, errors propagate" behaviour. Bounded by MaxAttempts;
	// each attempt counts the engine's name in a
	// `fallback_attempts` field on the access log.
	Fallback FallbackConfig `koanf:"fallback"`
}

// FallbackConfig drives the optional engine fallback chain. Activated
// only when an engine returns a 5xx, signals breaker-open, or
// reports a transport-level error. Non-engine-specific errors (400
// from the proxy itself, multipart parse failure) NEVER trigger
// fallback — those are caller-side problems, not engine flakes.
type FallbackConfig struct {
	// Enabled toggles the chain. Default false preserves the
	// "errors propagate, no retry" semantics every existing
	// deployment relies on.
	Enabled bool `koanf:"enabled"`
	// MaxAttempts caps the total attempt count (primary + fallbacks).
	// 0 → 2 (primary + one fallback). Operators bump higher when
	// they have many sibling engines and a true high-availability
	// requirement.
	MaxAttempts int `koanf:"max_attempts"`
}

// MimedetectConfig configures the MIME resolution layer.
//
// ExtensionOverrides extends or replaces the built-in CFB-disambiguation
// table that maps a filename extension (lowercase, leading dot — e.g.,
// ".msg") to a canonical MIME type. Operator-supplied entries are
// merged on top of the built-in defaults, so listing only the keys you
// care about is safe; listing a key that already exists replaces it.
//
// Keys MUST be lowercase, leading-dot, single-suffix
// (e.g., ".msg" — not "msg" or ".tar.gz"). Values MUST look like a
// MIME type ("type/subtype"); we don't enforce IANA registration, but
// the format is checked via regex so a typo fails the bootstrap
// instead of silently misrouting at request time.
type MimedetectConfig struct {
	ExtensionOverrides map[string]string `koanf:"extension_overrides"`
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
	// CompatType selects the adapter wire-protocol family.
	// `compat_type_valid` is the custom validator (registered in
	// Validate) that consults the compatTypes slice — keeps the
	// allowed-list in lockstep with the Compat* consts without a
	// hand-maintained tag literal.
	CompatType CompatType `koanf:"compat_type" validate:"required_if=Enable true,omitempty,compat_type_valid"`
	URL        string     `koanf:"url" validate:"required_if=Enable true,omitempty,url"`
	APIKeyEnv  string     `koanf:"api_key_env"`
	APIKey     Secret     `koanf:"-"`
	// AuthHeader is the outbound HTTP header name to stamp with the
	// engine's API key. Defaults supplied per compat_type by Validate
	// when empty.
	AuthHeader string `koanf:"auth_header" validate:"omitempty"`
	// AuthScheme adjusts how the API key is rendered. "raw" uses the
	// key value as-is (e.g., "X-Api-Key: abc123"); "bearer" prepends
	// "Bearer " (e.g., "Authorization: Bearer abc123").
	AuthScheme AuthScheme `koanf:"auth_scheme" validate:"omitempty,oneof=raw bearer"`
	// AuthHeaders is the multi-stamp auth list. When non-empty it
	// supplements the singular AuthHeader/APIKeyEnv/AuthScheme triple
	// — every entry here AND the singular form (if also set) get
	// stamped on every outbound request. Use this when a backend
	// requires multiple credential headers (X-Api-Key +
	// Authorization, X-Tenant-Token + X-Api-Key, etc.). Header names
	// are deduplicated case-insensitively at config-load time; a
	// duplicate fails the bootstrap so silent overrides cannot happen.
	//
	// Migration: an old single-header config remains valid forever.
	// applyEngineDefaults synthesises a one-element AuthHeaders slice
	// from the singular fields when the slice is empty so adapters
	// can read AuthHeaders uniformly without case-splitting.
	AuthHeaders    []AuthHeaderConfig `koanf:"auth_headers" validate:"omitempty,dive"`
	RequestTimeout time.Duration      `koanf:"request_timeout"`
	HealthPath     string             `koanf:"health_path"`
	MimeTypes      []string           `koanf:"mime_types"`
	// Extensions is the case-insensitive filename-extension allowlist
	// consulted when routing.strategy enables the extension phase
	// (`extension`, `mime_then_extension`, `extension_then_mime`).
	// Values may include or omit the leading dot; the registry
	// normalises both to ".pdf" form. Empty list means the engine
	// abstains from extension dispatch — operator opt-in,
	// symmetric with the mime_types allowlist semantics.
	Extensions     []string          `koanf:"extensions"`
	Paths          EnginePathsConfig `koanf:"paths"`
	ForwardOptions map[string]string `koanf:"forward_options"`
	HTTP           HTTPClientConfig  `koanf:"http"`
	Breaker        BreakerConfig     `koanf:"breaker"`
	RateLimit      RateLimitConfig   `koanf:"rate_limit"`
	// Observability holds per-engine overrides for the global
	// observability.upstream defaults. Unset fields inherit the global
	// value at startup (see applyEngineDefaults).
	Observability EngineObservabilityConfig `koanf:"observability"`
}

// AuthHeaderConfig is one entry in the multi-stamp auth list. Each
// entry stamps an INDEPENDENT outbound HTTP header; the engine
// adapter walks the slice in order and applies each.
//
// Fields:
//   - Header: outbound header name (regex-validated against
//     `authHeaderRE`, same charset as the singular AuthHeader).
//   - APIKeyEnv: env var name to read the credential from.
//   - APIKey: resolved at startup by resolveSecrets; never YAML-borne.
//   - Scheme: "raw" | "bearer"; empty defaults to "raw" (matches the
//     singular AuthScheme default).
type AuthHeaderConfig struct {
	Header    string     `koanf:"header" validate:"required"`
	APIKeyEnv string     `koanf:"api_key_env" validate:"required"`
	APIKey    Secret     `koanf:"-"`
	Scheme    AuthScheme `koanf:"scheme" validate:"omitempty,oneof=raw bearer"`
}

// EngineObservabilityConfig overrides ObservabilityConfig.Upstream
// fields for a single engine. Each pointer-typed field is "unset =
// inherit global". We use *bool / *int instead of zero-value sentinels
// so that a user can explicitly disable an engine's upstream logging
// even when the global default has it on.
type EngineObservabilityConfig struct {
	Enabled              *bool   `koanf:"enabled"`
	BodySnippetBytes     *int    `koanf:"body_snippet_bytes"`
	Level4xx             *string `koanf:"level_4xx"`
	Level5xx             *string `koanf:"level_5xx"`
	BodyLogBytes         *int    `koanf:"body_log_bytes"`
	FullCapture          *bool   `koanf:"full_capture"`
	FullCaptureBodyBytes *int    `koanf:"full_capture_body_bytes"`
}

// EnginePathsConfig overrides per-compat default paths. The map is
// keyed on a logical path name owned by the adapter package (e.g.
// docling exports `PathConvertFile`, external exports `PathProcess`).
// Empty map / missing key → the adapter uses its built-in default.
//
// Pre-v0.8.0 this was a fixed struct that namespace-prefixed each
// key with its compat type (`docling_convert_file`, `external_process`,
// `tika_text`). The prefix was redundant — an engine is exactly one
// compat type — and added an N×M cell to the schema for every new
// compat-type or path-key axis. Adding a fifth compat now changes
// one file (the new package) instead of three.
//
// Schema example:
//
//	engines:
//	  main-docling:
//	    paths:
//	      convert_file:   /api/v2/extract
//	      convert_source: /api/v2/source
type EnginePathsConfig = map[string]string

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
	ProxyAPIKeysEnv   string   `koanf:"proxy_api_keys_env"`
	ProxyAPIKeyHeader string   `koanf:"proxy_api_key_header"`
	ProxyAPIKeys      []Secret `koanf:"-"`
	RequireAPIKey     bool     `koanf:"require_api_key"`
	// ProxyAPIKeyFingerprintPepperEnv names the env var holding a
	// per-process secret HMAC'd into caller-API-key fingerprints
	// stored in Redis. With this set, an attacker who exfils Redis
	// cannot recover the API keys via offline rainbow tables
	// against the fingerprint — they would need to ALSO steal the
	// pepper from the proxy process. Empty value (the legacy
	// default) falls back to plain SHA-256 fingerprints, which
	// keeps existing deployments working. (C-28 from REVIEW-FAANG.md.)
	ProxyAPIKeyFingerprintPepperEnv string     `koanf:"proxy_api_key_fingerprint_pepper_env"`
	ProxyAPIKeyFingerprintPepper    Secret     `koanf:"-"`
	TrustedProxies                  []string   `koanf:"trusted_proxies"`
	CORS                            CORSConfig `koanf:"cors"`
	SSRF                            SSRFConfig `koanf:"ssrf"`
}

// SSRFConfig tunes the /v1/convert/source policy.
//
//	DNSCacheTTL   — how long to remember successful DNS resolutions.
//	                0 disables caching (every request hits the
//	                resolver). 60s is a good default: short enough
//	                that an attacker who poisons a record can't keep
//	                the proxy stuck on a stale IP for long, long
//	                enough that bursty workloads from the same host
//	                amortise the lookup.
//	DNSCacheMax   — max entries before LRU-ish eviction (oldest by
//	                expiry). Default 256.
//	BlockedCIDRs  — extra CIDRs to add to the built-in deny list.
//	                These are VALIDATED at Load() time so a typo
//	                like "10.0.0.0/333" fails fast instead of
//	                panicking at request time.
type SSRFConfig struct {
	DNSCacheTTL  time.Duration `koanf:"dns_cache_ttl"`
	DNSCacheMax  int           `koanf:"dns_cache_max" validate:"gte=0"`
	BlockedCIDRs []string      `koanf:"blocked_cidrs"`
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
	Log      LogConfig         `koanf:"log"`
	Metrics  MetricsConfig     `koanf:"metrics"`
	Tracing  TracingConfig     `koanf:"tracing"`
	Upstream UpstreamLogConfig `koanf:"upstream"`
	Health   HealthConfig      `koanf:"health"`
}

// HealthConfig controls the /readyz probe behaviour.
//
//	MaxParallel — how many engine probes to fan out concurrently.
//	              The previous hard-coded value of 8 throttled
//	              deployments with more engines into serial probes,
//	              risking kubelet timeout. Default 16 covers most
//	              realistic engine counts; bump for >16-engine
//	              deployments.
//	Timeout     — overall ceiling for the readiness probe. Each
//	              engine's Health() inherits a derived deadline.
//	              Default 5s.
type HealthConfig struct {
	MaxParallel int           `koanf:"max_parallel" validate:"gte=0"`
	Timeout     time.Duration `koanf:"timeout"`
}

type LogConfig struct {
	Level     string `koanf:"level" validate:"omitempty,oneof=trace debug info warn error fatal panic"`
	Format    string `koanf:"format" validate:"omitempty,oneof=json console"`
	Sampling  bool   `koanf:"sampling"`
	AddCaller bool   `koanf:"add_caller"`

	// Timezone for log timestamps. Accepts any value parseable by
	// time.LoadLocation: IANA names ("Europe/Istanbul", "America/New_York"),
	// "UTC", or "Local". Empty = UTC. Invalid values fall back to UTC
	// with a startup warning (we never crash on misconfigured logging).
	Timezone string `koanf:"timezone"`
	// TimeFormat selects the timestamp serialisation. Accepts:
	//   "RFC3339Nano" (default, ISO-8601 + nanos + tz offset)
	//   "RFC3339"
	//   "unix"        (seconds since epoch)
	//   "unix_ms"     (milliseconds)
	//   "unix_micro"
	//   any Go time-layout literal (e.g., "2006-01-02 15:04:05.000")
	TimeFormat string `koanf:"time_format"`
	// SilencePaths is the list of request paths the AccessLog
	// middleware skips entirely. Use this to mute readiness/health
	// probes that would otherwise dominate the log stream. Default
	// empty = log everything; recommended value:
	// ["/healthz","/readyz","/metrics"].
	SilencePaths []string `koanf:"silence_paths"`
	// SilenceEngineHealth, when true, mutes the outbound debug log
	// emitted by the instrumented HTTP transport for engine.Health()
	// probes. The corresponding Prometheus counters still fire so
	// dashboards remain accurate; only the per-probe debug line is
	// dropped. Useful in debug mode when readiness fires every few
	// seconds.
	SilenceEngineHealth bool `koanf:"silence_engine_health"`
	// FieldNames remaps zerolog's canonical field keys. Empty values
	// keep the zerolog defaults (level, message, time, error). Useful
	// for integrating with log shippers that expect specific keys
	// (e.g., GCP Cloud Logging wants "severity").
	FieldNames LogFieldNames `koanf:"field_names"`
	// StaticFields are appended to EVERY log line via logger.With().
	// Common use: stamp service name, environment, k8s pod identity.
	StaticFields map[string]string `koanf:"static_fields"`
}

// LogFieldNames overrides the zerolog default field names. Any value
// left empty keeps the upstream default.
type LogFieldNames struct {
	Level     string `koanf:"level"`
	Message   string `koanf:"message"`
	Timestamp string `koanf:"timestamp"`
	Error     string `koanf:"error"`
}

// UpstreamLogConfig governs how non-2xx engine responses are surfaced
// in proxy logs and how much of the outbound request/response is
// captured for debugging. These are GLOBAL defaults; each engine may
// override individual fields via EngineConfig.Observability.
type UpstreamLogConfig struct {
	// Enabled toggles all upstream observability hooks. When false the
	// proxy falls back to v0.1.x behaviour (only transport-level errors
	// logged).
	Enabled bool `koanf:"enabled"`
	// BodySnippetBytes caps how many bytes of a 4xx/5xx response body
	// are captured into the warn/error log line. 0 disables the
	// snippet (only status code logged). The captured prefix is
	// peek-restored — the caller still sees the full body.
	BodySnippetBytes int `koanf:"body_snippet_bytes" validate:"gte=0"`
	// Level4xx and Level5xx control the zerolog level used to emit the
	// upstream-error line. Operators dial these down to "info" when a
	// flaky backend is expected to return 4xx noise.
	Level4xx string `koanf:"level_4xx" validate:"omitempty,oneof=trace debug info warn error"`
	Level5xx string `koanf:"level_5xx" validate:"omitempty,oneof=trace debug info warn error"`
	// BodyLogBytes (default 0) opts an operator into debug-level
	// outbound REQUEST body logging via the outbound RoundTripper. The
	// snippet is base64-encoded to stay binary-safe. WARNING: enabling
	// this in production with sensitive uploads will write file
	// contents to the log stream — keep small and prefer ephemeral
	// debugging sessions.
	BodyLogBytes int `koanf:"body_log_bytes" validate:"gte=0"`
	// FullCapture turns on per-request audit logging that emits a
	// dedicated `request_full_capture` log line carrying the full
	// inbound + outbound request/response surface. DEFAULT OFF —
	// enabling this in production WILL leak PII / secrets to logs;
	// the redactor strips known credential headers
	// (Authorization, X-Api-Key, the configured ProxyAPIKeyHeader,
	// every engine's configured auth_header) but cannot detect
	// secrets embedded in bodies. Run with a short-lived debug log
	// sink, never the primary log stream. Every captured line carries
	// `sensitive_capture: true` so log shippers can route it
	// separately.
	FullCapture bool `koanf:"full_capture"`
	// FullCaptureBodyBytes is the truncation cap applied to captured
	// request + response bodies. 0 keeps bodies redacted entirely
	// (only headers + status + size are logged). Effective max is
	// 64 KiB regardless of operator setting — the capture pipeline
	// hard-caps to keep one bad upload from filling the log shipper.
	FullCaptureBodyBytes int `koanf:"full_capture_body_bytes" validate:"gte=0,lte=65536"`
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
			// WriteTimeout MUST exceed RequestTimeout by at least the
			// 2 s slack enforced in validateTimeoutHierarchy so the
			// response writer is not torn down mid-stream when a slow
			// handler exhausts its per-request budget. A 30 s gap
			// covers TCP teardown + body finalisation under load AND
			// leaves operators headroom to bump RequestTimeout up to
			// 5 m 28 s before they have to touch WriteTimeout too.
			WriteTimeout:   5*time.Minute + 30*time.Second,
			IdleTimeout:    120 * time.Second,
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
			DefaultEngine: "",
			Strategy:      engine.StrategyMimeThenExt,
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
			SSRF: SSRFConfig{
				DNSCacheTTL: 60 * time.Second,
				DNSCacheMax: 256,
			},
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
			Log: LogConfig{
				Level:      "info",
				Format:     "json",
				AddCaller:  false,
				Timezone:   "UTC",
				TimeFormat: "RFC3339Nano",
			},
			Metrics: MetricsConfig{Enabled: true, Path: "/metrics"},
			Tracing: TracingConfig{Enabled: false, SampleRatio: 0.1, ServiceName: "owui-cee-proxy"},
			Upstream: UpstreamLogConfig{
				Enabled:          true,
				BodySnippetBytes: 256,
				Level4xx:         "warn",
				Level5xx:         "error",
				BodyLogBytes:     0,
			},
			Health: HealthConfig{
				MaxParallel: 16,
				Timeout:     5 * time.Second,
			},
		},
	}
}

// DefaultEngineConfig returns a baseline EngineConfig used as a
// per-entry merge layer. Exported so the migration helper can reuse it.
func DefaultEngineConfig() EngineConfig {
	return EngineConfig{
		Enable:         false,
		AuthScheme:     AuthSchemeRaw,
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

// DefaultConfigPath returns the YAML config path the binary should
// load when the operator didn't pass `-config`. Order of resolution:
//  1. OWUI_PROXY_CONFIG env var (matches the loader's prefix scheme)
//  2. configs/config.example.yaml (repo-relative fallback for dev)
//
// All env-var access for proxy startup MUST flow through this package
// per the CLAUDE.md invariant ("don't call os.Getenv outside
// internal/config") so CI/tests can override deterministically.
func DefaultConfigPath() string {
	if v := os.Getenv("OWUI_PROXY_CONFIG"); v != "" {
		return v
	}
	return "configs/config.example.yaml"
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
				if ec.AuthScheme == AuthSchemeRaw {
					ec.AuthScheme = AuthSchemeBearer
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
		// Multi-stamp: each AuthHeaders entry has its own env var.
		// Resolve every one; missing env vars surface in AuthWarnings
		// the same way the singular fields do.
		for i, ah := range ec.AuthHeaders {
			if ah.APIKeyEnv != "" {
				ah.APIKey = Secret(os.Getenv(ah.APIKeyEnv))
				ec.AuthHeaders[i] = ah
			}
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
	if c.Security.ProxyAPIKeyFingerprintPepperEnv != "" {
		c.Security.ProxyAPIKeyFingerprintPepper = Secret(os.Getenv(c.Security.ProxyAPIKeyFingerprintPepperEnv))
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
	// compat_type_valid: registered before v.Struct(c) so the
	// EngineConfig.CompatType tag resolves at struct-validation
	// time. The set of accepted values comes from compatTypes (the
	// SoT for the validator) — no hand-maintained `oneof=` literal.
	_ = v.RegisterValidation("compat_type_valid", func(fl validator.FieldLevel) bool {
		val := CompatType(fl.Field().String())
		for _, c := range compatTypes {
			if val == c {
				return true
			}
		}
		return false
	})
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
	if err := validateSSRFConfig(c); err != nil {
		return err
	}
	if err := validateTasksConfig(c); err != nil {
		return err
	}
	if err := validateServerTLS(c); err != nil {
		return err
	}
	if err := validateTimeoutHierarchy(c); err != nil {
		return err
	}
	if err := validateResolvedSecrets(c); err != nil {
		return err
	}
	if err := validateMimedetect(c); err != nil {
		return err
	}
	return nil
}

// validateResolvedSecrets scans every resolved engine credential for
// bytes outside the visible-ASCII window (0x20..0x7E, plus TAB).
// Operators sometimes paste a credential into the wrong env var, or
// the env var picks up a trailing CR/LF from a shell expansion; the
// outbound HTTP layer would otherwise carry the invalid bytes
// straight into the engine backend's header parser. CR/LF in
// particular is the CWE-93 header-injection primitive.
//
// We fail the bootstrap rather than silently strip — operators get
// a clear error pointing at which key is malformed, instead of
// debugging stochastic 502s in production.
//
// Public-engine entries (no env var configured, so APIKey == "") are
// skipped: empty is fine, that is the no-auth path.
func validateResolvedSecrets(c *Config) error {
	for name, ec := range c.Engines {
		if !ec.Enable {
			continue
		}
		if err := scanAuthValue(name, "api_key", string(ec.APIKey)); err != nil {
			return err
		}
		for i, ah := range ec.AuthHeaders {
			fieldName := fmt.Sprintf("auth_headers[%d]", i)
			if err := scanAuthValue(name, fieldName, string(ah.APIKey)); err != nil {
				return err
			}
		}
	}
	for i, k := range c.Security.ProxyAPIKeys {
		fieldName := fmt.Sprintf("security.proxy_api_keys[%d]", i)
		if err := scanAuthValue("", fieldName, string(k)); err != nil {
			return err
		}
	}
	return nil
}

// scanAuthValue rejects any byte outside the visible-ASCII window
// (0x20..0x7E, plus TAB) in the resolved credential. Returns nil for
// an empty value — that is the unconfigured / public-engine path
// which validateResolvedSecrets's caller has already chosen to
// accept.
func scanAuthValue(engine, field, val string) error {
	if val == "" {
		return nil
	}
	for i := 0; i < len(val); i++ {
		b := val[i]
		if b == '\t' {
			continue
		}
		if b < 0x20 || b > 0x7E {
			scope := field
			if engine != "" {
				scope = fmt.Sprintf("engines.%s.%s", engine, field)
			}
			return fmt.Errorf("%s: resolved credential contains a non-printable byte at offset %d (0x%02x); CR/LF or stray control bytes in env vars are rejected to prevent header injection",
				scope, i, b)
		}
	}
	return nil
}

// validateMimedetect enforces the format of the operator-supplied
// extension override map. Both halves of every entry must look right
// at startup — a typo here translates to a silent routing
// misbehaviour at request time, which is exactly what the override
// map was meant to fix in the first place.
func validateMimedetect(c *Config) error {
	for k, v := range c.Mimedetect.ExtensionOverrides {
		if !extensionOverrideKeyRE.MatchString(k) {
			return fmt.Errorf(
				"mimedetect.extension_overrides: invalid key %q; want lowercase, leading-dot, single-segment extension like %q",
				k, ".msg",
			)
		}
		if !mimeValueRE.MatchString(v) {
			return fmt.Errorf(
				"mimedetect.extension_overrides[%q]: invalid MIME %q; want a bare %q form without parameters",
				k, v, "type/subtype",
			)
		}
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
		// Multi-stamp validation: each entry needs a valid header
		// regex; the resolved set (singular + multi) must dedup
		// case-insensitively. Duplicates would silently overwrite
		// each other on the wire (http.Header.Set semantics) — fail
		// the bootstrap so the operator sees the conflict.
		seen := make(map[string]string, 1+len(ec.AuthHeaders))
		if ec.AuthHeader != "" {
			seen[strings.ToLower(ec.AuthHeader)] = ec.AuthHeader
		}
		for i, ah := range ec.AuthHeaders {
			if !authHeaderRE.MatchString(ah.Header) {
				return fmt.Errorf("engines.%s.auth_headers[%d].header %q: invalid header name; must match %s",
					name, i, ah.Header, authHeaderRE)
			}
			key := strings.ToLower(ah.Header)
			if prev, dup := seen[key]; dup {
				return fmt.Errorf("engines.%s.auth_headers: duplicate header %q (also declared as %q); each header name may appear once across auth_header + auth_headers",
					name, ah.Header, prev)
			}
			seen[key] = ah.Header
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
func acceptedFacades(compat CompatType) []string {
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
func AcceptedFacades(compat CompatType) []string { return acceptedFacades(compat) }

// EffectiveAuthSpecs returns the flattened auth-header list the
// adapter should walk on every outbound request. The singular
// AuthHeader/APIKeyEnv/AuthScheme triple is prepended (when set);
// AuthHeaders entries follow in declared order. Adapters iterate the
// result and stamp each header — the multi-stamp contract.
//
// Result MAY be empty (engine has no auth configured at all); MAY
// contain duplicate header names if validateAuthHeaders is not
// invoked first (it is, at config load).
func (e EngineConfig) EffectiveAuthSpecs() []AuthHeaderConfig {
	specs := make([]AuthHeaderConfig, 0, 1+len(e.AuthHeaders))
	if e.AuthHeader != "" {
		specs = append(specs, AuthHeaderConfig{
			Header:    e.AuthHeader,
			APIKeyEnv: e.APIKeyEnv,
			APIKey:    e.APIKey,
			Scheme:    e.AuthScheme,
		})
	}
	specs = append(specs, e.AuthHeaders...)
	return specs
}

// ResolveUpstream merges the per-engine observability override on top
// of the global upstream defaults. Each EngineObservabilityConfig
// field is "nil = inherit global". The returned value is what the
// adapter, RoundTripper, and handler stack should consult — never
// read EngineObservabilityConfig directly.
func (e EngineConfig) ResolveUpstream(global UpstreamLogConfig) UpstreamLogConfig {
	out := global
	if e.Observability.Enabled != nil {
		out.Enabled = *e.Observability.Enabled
	}
	if e.Observability.BodySnippetBytes != nil {
		out.BodySnippetBytes = *e.Observability.BodySnippetBytes
	}
	if e.Observability.Level4xx != nil {
		out.Level4xx = *e.Observability.Level4xx
	}
	if e.Observability.Level5xx != nil {
		out.Level5xx = *e.Observability.Level5xx
	}
	if e.Observability.BodyLogBytes != nil {
		out.BodyLogBytes = *e.Observability.BodyLogBytes
	}
	if e.Observability.FullCapture != nil {
		out.FullCapture = *e.Observability.FullCapture
	}
	if e.Observability.FullCaptureBodyBytes != nil {
		out.FullCaptureBodyBytes = *e.Observability.FullCaptureBodyBytes
	}
	return out
}

// AuthWarnings returns non-fatal misconfiguration hints discovered at
// load time. The result is intended to be logged at startup (one
// warn per entry) so operators see them in the bootstrap output
// instead of debugging silent 401s in production.
//
// The proxy DOES NOT fail to start on these — an engine may legitimately
// be public, or an operator may be staging a config before rotating
// secrets. We only surface the asymmetric / suspicious cases.
func (c *Config) AuthWarnings() []string {
	var out []string
	for name, ec := range c.Engines {
		if !ec.Enable {
			continue
		}
		// auth_header set, api_key_env unset → operator intended to
		// stamp a header but never declared which env var to read.
		if ec.AuthHeader != "" && ec.APIKeyEnv == "" {
			out = append(out, fmt.Sprintf(
				"engine %q declares auth_header=%q but no api_key_env; outbound requests will not carry an API key",
				name, ec.AuthHeader,
			))
			continue
		}
		// api_key_env named but env var unset / empty → silent
		// resolution failure (this is the user's actual complaint).
		if ec.APIKeyEnv != "" && ec.APIKey == "" {
			out = append(out, fmt.Sprintf(
				"engine %q references api_key_env=%q but the environment variable is empty; outbound requests will not carry an API key",
				name, ec.APIKeyEnv,
			))
		}
		// Multi-stamp: same checks per entry.
		for i, ah := range ec.AuthHeaders {
			if ah.APIKeyEnv != "" && ah.APIKey == "" {
				out = append(out, fmt.Sprintf(
					"engine %q auth_headers[%d] references api_key_env=%q but the environment variable is empty; this stamp will be skipped at runtime",
					name, i, ah.APIKeyEnv,
				))
			}
		}
	}
	return out
}

// validateEnginePaths enforces that docling-external engines declare
// both endpoint paths (or rely on built-in defaults — declared empty
// means use the adapter default).
// validateTasksConfig pulls the async-task validation out of
// Validate() so each helper stays under the cyclomatic-complexity
// budget. Only meaningful when tasks.enabled=true; otherwise the
// fields are dormant and not asserted.
func validateTasksConfig(c *Config) error {
	if !c.Tasks.Enabled {
		return nil
	}
	if c.Tasks.RedisURL == "" {
		return fmt.Errorf("tasks.enabled=true but %s env is empty", c.Tasks.RedisURLEnv)
	}
	if c.Tasks.MaxTotalBytes > 0 && c.Tasks.MaxBlobBytes > c.Tasks.MaxTotalBytes {
		return fmt.Errorf("tasks.max_blob_bytes (%d) must be <= max_total_bytes (%d)",
			c.Tasks.MaxBlobBytes, c.Tasks.MaxTotalBytes)
	}
	if c.Tasks.TaskTimeout < 0 {
		return fmt.Errorf("tasks.task_timeout must be non-negative")
	}
	for q, w := range c.Tasks.QueueWeights {
		if w < 0 {
			return fmt.Errorf("tasks.queue_weights[%q] must be non-negative", q)
		}
	}
	return nil
}

// validateTimeoutHierarchy enforces the operator-facing invariant
// that the proxy's timeout knobs are layered correctly:
//
//	max(engines.*.request_timeout) ≤ server.request_timeout
//	server.request_timeout         ≤ server.write_timeout − slack
//
// When the hierarchy inverts, the proxy still RUNS, but the chi
// `mw.Timeout(server.request_timeout)` cancels the context before
// a slower engine's per-engine timeout can fire — that turns a
// healthy-but-slow backend into a cancelled-by-proxy failure, which
// the breaker counts as a transport error and TRIPS, taking the
// engine offline. Operators don't realise: they see breaker open,
// they don't see "I configured my timeouts in the wrong order".
//
// We fail the bootstrap with a precise diff so the operator can fix
// the YAML rather than debug a stochastic breaker trip in
// production. The 2 s slack between request_timeout and write_timeout
// covers TCP teardown + body finalisation under load; smaller deltas
// race the http.Server.WriteTimeout fire against the in-flight
// response stream.
//
// Engines with `RequestTimeout == 0` are skipped — the adapter
// inherits `DefaultRequestTimeout` (120 s) which is bounded.
func validateTimeoutHierarchy(c *Config) error {
	const writeTimeoutSlack = 2 * time.Second

	var maxEngine time.Duration
	var maxEngineName string
	for name, ec := range c.Engines {
		if !ec.Enable {
			continue
		}
		if ec.RequestTimeout > maxEngine {
			maxEngine = ec.RequestTimeout
			maxEngineName = name
		}
	}

	if maxEngine > 0 && c.Server.RequestTimeout > 0 && maxEngine > c.Server.RequestTimeout {
		return fmt.Errorf(
			"timeout hierarchy: engines.%s.request_timeout=%s exceeds server.request_timeout=%s; the chi Timeout middleware would cancel the context before the engine's own ceiling fires, tripping the breaker on a healthy backend",
			maxEngineName, maxEngine, c.Server.RequestTimeout,
		)
	}
	if c.Server.WriteTimeout > 0 && c.Server.RequestTimeout > 0 &&
		c.Server.RequestTimeout+writeTimeoutSlack > c.Server.WriteTimeout {
		return fmt.Errorf(
			"timeout hierarchy: server.request_timeout=%s requires server.write_timeout ≥ %s (current %s); without the %s slack the response writer is torn down mid-stream",
			c.Server.RequestTimeout, c.Server.RequestTimeout+writeTimeoutSlack, c.Server.WriteTimeout, writeTimeoutSlack,
		)
	}
	return nil
}

// validateServerTLS checks the server-side TLS coherence. Split out
// to keep Validate() under the cyclomatic budget. The cert+key
// pairing rule is the only one that matters here; struct-level
// validator tags handle the enum/range constraints.
func validateServerTLS(c *Config) error {
	if c.Server.TLS.Enabled && (c.Server.TLS.CertFile == "" || c.Server.TLS.KeyFile == "") {
		return fmt.Errorf("server.tls.enabled=true requires cert_file and key_file")
	}
	return nil
}

// validateSSRFConfig parses every operator-supplied blocked CIDR
// at startup so a typo fails the bootstrap instead of panicking at
// the first /v1/convert/source request. The built-in blocked CIDR
// list is hard-coded constants and stays panic-on-mistake (that's
// a programmer error, not an operator one).
func validateSSRFConfig(c *Config) error {
	if c.Security.SSRF.DNSCacheTTL < 0 {
		return fmt.Errorf("security.ssrf.dns_cache_ttl must be >= 0")
	}
	for _, raw := range c.Security.SSRF.BlockedCIDRs {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if _, _, err := net.ParseCIDR(raw); err != nil {
			return fmt.Errorf("security.ssrf.blocked_cidrs: invalid CIDR %q: %w", raw, err)
		}
	}
	return nil
}

func validateEnginePaths(c *Config) error {
	// Currently the adapter defaults handle empty path overrides, so
	// no per-compat path enforcement is required at validation time.
	// Reserved for future use (e.g., when a compat_type lacks a
	// canonical default path).
	_ = c
	return nil
}
