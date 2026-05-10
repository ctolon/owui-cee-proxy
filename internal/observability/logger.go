// Package observability initialises the structured logger, Prometheus
// metrics, and OpenTelemetry tracer. Components depend on these via
// small interfaces so tests can swap fakes in.
package observability

import (
	"io"
	"os"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/ctolon/owui-cee-proxy/internal/config"
)

// NewLogger creates a zerolog.Logger writing to stdout. Format=json
// is the production default; format=console is intended for local dev.
func NewLogger(cfg config.LogConfig) zerolog.Logger {
	level := parseLevel(cfg.Level)
	zerolog.TimeFieldFormat = time.RFC3339Nano
	var w io.Writer = os.Stdout
	if strings.EqualFold(cfg.Format, "console") {
		w = zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: time.RFC3339}
	}
	logger := zerolog.New(w).Level(level).With().Timestamp()
	if cfg.AddCaller {
		logger = logger.Caller()
	}
	return logger.Logger()
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
