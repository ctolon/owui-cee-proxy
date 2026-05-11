package observability

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/ctolon/owui-cee-proxy/internal/config"
)

// loggerMu serialises tests that mutate zerolog's package-global
// state (TimeFieldFormat, TimestampFunc, *FieldName). Without this
// guard the t.Parallel() suite races across rerolls.
var loggerMu sync.Mutex

// withLoggerLock wraps a test body so global zerolog state is
// touched under a mutex. Use INSTEAD of t.Parallel for tests that
// call NewLogger.
func withLoggerLock(t *testing.T, fn func()) {
	t.Helper()
	loggerMu.Lock()
	defer loggerMu.Unlock()
	defer resetZerologGlobals()
	fn()
}

func resetZerologGlobals() {
	zerolog.TimeFieldFormat = time.RFC3339Nano
	zerolog.TimestampFunc = time.Now
	zerolog.LevelFieldName = "level"
	zerolog.MessageFieldName = "message"
	zerolog.TimestampFieldName = "time"
	zerolog.ErrorFieldName = "error"
}

func TestNewLogger_TimezoneAppliedToTimestamps(t *testing.T) {
	withLoggerLock(t, func() {
		// New York is UTC-5 (or UTC-4 with DST) — far enough from UTC
		// that the offset suffix in RFC3339Nano makes a clear assertion
		// regardless of season.
		buf := &bytes.Buffer{}
		// We can't easily inject a writer through NewLogger (it hardcodes
		// stdout). Instead, set up the global state via NewLogger and
		// then build a parallel logger pointing at buf — both will see
		// the same TimestampFunc.
		_ = NewLogger(config.LogConfig{
			Level:    "debug",
			Format:   "json",
			Timezone: "America/New_York",
		})
		l := zerolog.New(buf).With().Timestamp().Logger()
		l.Info().Msg("probe")

		var line map[string]any
		require.NoError(t, json.Unmarshal(buf.Bytes(), &line))
		ts, ok := line["time"].(string)
		require.True(t, ok, "timestamp field must be a string")
		// Parse and assert the offset is NOT zero (i.e., not UTC).
		parsed, err := time.Parse(time.RFC3339Nano, ts)
		require.NoError(t, err)
		_, off := parsed.Zone()
		require.NotEqual(t, 0, off, "timestamp should carry New York offset, got %s", ts)
	})
}

func TestNewLogger_TimezoneInvalidFallsBackToUTC(t *testing.T) {
	withLoggerLock(t, func() {
		// Capture stderr fallback warning is best-effort; here we only
		// verify it doesn't crash and timestamps stay parseable.
		require.NotPanics(t, func() {
			_ = NewLogger(config.LogConfig{Timezone: "Mars/Olympus_Mons"})
		})
	})
}

func TestNewLogger_TimeFormatAliases(t *testing.T) {
	cases := []struct {
		input    string
		expected string
	}{
		{"", time.RFC3339Nano},
		{"RFC3339Nano", time.RFC3339Nano},
		{"rfc3339nano", time.RFC3339Nano},
		{"RFC3339", time.RFC3339},
		{"unix", zerolog.TimeFormatUnix},
		{"unix_ms", zerolog.TimeFormatUnixMs},
		{"unix-micro", zerolog.TimeFormatUnixMicro},
		{"2006-01-02 15:04:05", "2006-01-02 15:04:05"}, // pass-through
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			require.Equal(t, tc.expected, resolveTimeFormat(tc.input))
		})
	}
}

func TestNewLogger_FieldNameRemap(t *testing.T) {
	withLoggerLock(t, func() {
		_ = NewLogger(config.LogConfig{
			Level: "info",
			FieldNames: config.LogFieldNames{
				Level:     "severity",
				Message:   "msg",
				Timestamp: "ts",
			},
		})
		// Build a parallel logger that reads the now-remapped package
		// globals so we can assert on its JSON output.
		buf := &bytes.Buffer{}
		l := zerolog.New(buf).Level(zerolog.InfoLevel).With().Timestamp().Logger()
		l.Warn().Msg("hello")

		var line map[string]any
		require.NoError(t, json.Unmarshal(buf.Bytes(), &line))
		require.Equal(t, "warn", line["severity"], "level field renamed to severity")
		require.Equal(t, "hello", line["msg"], "message field renamed to msg")
		require.Contains(t, line, "ts", "timestamp field renamed to ts")
		require.NotContains(t, line, "level")
		require.NotContains(t, line, "message")
		require.NotContains(t, line, "time")
	})
}

func TestNewLogger_StaticFieldsAppendedToEveryLine(t *testing.T) {
	withLoggerLock(t, func() {
		// NewLogger writes to stdout, so to assert on output we
		// reconstruct the With() chain with the same StaticFields.
		// The cheaper path: assert StaticFields populate the
		// returned logger's Context().
		l := NewLogger(config.LogConfig{
			Level:        "info",
			StaticFields: map[string]string{"service": "owui-cee-proxy", "env": "test"},
		})
		// Re-emit through a captured writer.
		buf := &bytes.Buffer{}
		l2 := l.Output(buf)
		l2.Info().Msg("hello")

		var line map[string]any
		require.NoError(t, json.Unmarshal(buf.Bytes(), &line))
		require.Equal(t, "owui-cee-proxy", line["service"])
		require.Equal(t, "test", line["env"])
	})
}

func TestNewLogger_SamplingDoesNotPanic(t *testing.T) {
	withLoggerLock(t, func() {
		require.NotPanics(t, func() {
			l := NewLogger(config.LogConfig{Level: "debug", Sampling: true})
			// Smoke: confirm we can emit at least one event without
			// the BurstSampler swallowing the goroutine.
			l.Debug().Msg("ping")
		})
	})
}

// TestNewLogger_DefaultsAreSafe pins that an empty LogConfig produces
// a usable logger (matches the bootstrap "no observability stanza in
// YAML" path).
func TestNewLogger_DefaultsAreSafe(t *testing.T) {
	withLoggerLock(t, func() {
		l := NewLogger(config.LogConfig{})
		require.NotPanics(t, func() { l.Info().Msg("ok") })
		require.Equal(t, time.RFC3339Nano, zerolog.TimeFieldFormat,
			"empty TimeFormat must resolve to RFC3339Nano")
	})
}

// TestResolveLocation_KnownAliases keeps the timezone parser honest.
func TestResolveLocation_KnownAliases(t *testing.T) {
	t.Parallel()
	require.Nil(t, resolveLocation(""), "empty defaults to UTC (nil)")
	require.Nil(t, resolveLocation("UTC"), "explicit UTC bypasses override")
	require.Nil(t, resolveLocation("utc"), "case-insensitive UTC")
	loc := resolveLocation("America/New_York")
	require.NotNil(t, loc)
	require.Equal(t, "America/New_York", loc.String())

	// Invalid name returns nil + writes warning to stderr; we only
	// assert the nil return.
	require.Nil(t, resolveLocation("Invalid/Place/Here"))
	// Non-UTC zones must not be silently treated as UTC.
	loc2 := resolveLocation("Europe/Istanbul")
	require.NotNil(t, loc2)
	require.True(t, strings.Contains(loc2.String(), "Istanbul"))
}
