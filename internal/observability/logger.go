// Package observability initialises the structured logger, Prometheus
// metrics, and OpenTelemetry tracer. Components depend on these via
// small interfaces so tests can swap fakes in.
package observability

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/ctolon/owui-cee-proxy/internal/config"
)

// NewLogger creates a zerolog.Logger writing to stdout. Format=json
// is the production default; format=console is intended for local dev.
//
// Customisation honoured (all from config.LogConfig):
//   - Level         : verbosity threshold
//   - Format        : json | console
//   - Timezone      : IANA name | "UTC" | "Local" ("" = UTC). Bad
//                     values fall back to UTC silently (we never
//                     crash the proxy on a logging config typo).
//   - TimeFormat    : "RFC3339Nano" (default), "RFC3339", "unix",
//                     "unix_ms", "unix_micro", or any Go layout
//                     literal (e.g., "2006-01-02 15:04:05Z07:00").
//   - FieldNames    : remap zerolog's "level"/"message"/"time"/"error"
//                     for log shippers that expect different keys.
//   - StaticFields  : map appended to every log line via With().
//   - Sampling      : BurstSampler{5/sec} when true.
//   - AddCaller     : include file:line of each call.
//
// CAUTION: zerolog stores TimeFieldFormat, TimestampFunc, and
// FieldName overrides as PACKAGE GLOBALS. The last NewLogger call
// wins process-wide. Tests that build multiple loggers must be aware.
func NewLogger(cfg config.LogConfig) zerolog.Logger {
	level := parseLevel(cfg.Level)

	zerolog.TimeFieldFormat = resolveTimeFormat(cfg.TimeFormat)
	if loc := resolveLocation(cfg.Timezone); loc != nil {
		zerolog.TimestampFunc = func() time.Time { return time.Now().In(loc) }
	}
	applyFieldNames(cfg.FieldNames)

	var w io.Writer = os.Stdout
	if strings.EqualFold(cfg.Format, "console") {
		w = zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: time.RFC3339}
	}
	base := zerolog.New(w).Level(level)
	if cfg.Sampling {
		base = base.Sample(&zerolog.BurstSampler{
			Burst:  5,
			Period: time.Second,
		})
	}
	ctx := base.With().Timestamp()
	if cfg.AddCaller {
		ctx = ctx.Caller()
	}
	for k, v := range cfg.StaticFields {
		ctx = ctx.Str(k, v)
	}
	return ctx.Logger()
}

// resolveTimeFormat maps the operator-friendly aliases to the literal
// zerolog accepts in TimeFieldFormat. Custom layouts are passed
// through unchanged so power users can declare exotic shapes.
func resolveTimeFormat(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "rfc3339nano":
		return time.RFC3339Nano
	case "rfc3339":
		return time.RFC3339
	case "unix":
		return zerolog.TimeFormatUnix
	case "unix_ms", "unix-ms", "unixms":
		return zerolog.TimeFormatUnixMs
	case "unix_micro", "unix-micro":
		return zerolog.TimeFormatUnixMicro
	default:
		return s
	}
}

// resolveLocation parses tz into a *time.Location. Returns nil when
// the caller asked for the default (UTC) and the empty string —
// callers treat nil as "do not override TimestampFunc". On parse
// failure we ALSO return nil + emit a one-time warning to stderr so
// the operator notices but the proxy still boots.
func resolveLocation(tz string) *time.Location {
	tz = strings.TrimSpace(tz)
	if tz == "" || strings.EqualFold(tz, "utc") {
		// UTC is the default; let zerolog's default TimestampFunc
		// (time.Now()) handle it. We don't override.
		return nil
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr,
			"observability: invalid log.timezone %q (%v); falling back to UTC\n", tz, err)
		return nil
	}
	return loc
}

func applyFieldNames(fn config.LogFieldNames) {
	if fn.Level != "" {
		zerolog.LevelFieldName = fn.Level
	}
	if fn.Message != "" {
		zerolog.MessageFieldName = fn.Message
	}
	if fn.Timestamp != "" {
		zerolog.TimestampFieldName = fn.Timestamp
	}
	if fn.Error != "" {
		zerolog.ErrorFieldName = fn.Error
	}
}

func parseLevel(s string) zerolog.Level {
	switch strings.ToLower(s) {
	case "trace":
		return zerolog.TraceLevel
	case "debug":
		return zerolog.DebugLevel
	case "info", "":
		return zerolog.InfoLevel
	case "warn", "warning":
		return zerolog.WarnLevel
	case "error":
		return zerolog.ErrorLevel
	case "fatal":
		return zerolog.FatalLevel
	case "panic":
		return zerolog.PanicLevel
	default:
		return zerolog.InfoLevel
	}
}
