package middleware

import (
	"net/http"
	"runtime/debug"

	"github.com/rs/zerolog"
)

// PanicRecorder is the observability seam Recover hits when it
// catches a panic. Nil-safe in the sense that Recover requires a
// non-nil recorder; the composition root wires a Prometheus counter
// here. Tests pass a nop or a recording double.
type PanicRecorder interface {
	RecordPanic(path string)
}

// nopPanicRecorder is the safe default for tests / callers that
// construct Recover without observability wired. Counter-side
// regressions still show up in the log line, just not in metrics.
type nopPanicRecorder struct{}

func (nopPanicRecorder) RecordPanic(string) {}

// Recover catches panics from downstream handlers, logs the stack
// (as a STRING — `Bytes` would base64-encode it in JSON which makes
// the line unreadable, C-39 from REVIEW-FAANG.md) and 500s the
// caller. Also increments a `panics_total{path}` counter so
// dashboards can alert on regressions instead of relying on a log
// grep.
//
// recorder may be nil; the no-arg legacy callers get the nopPanic
// path automatically.
func Recover(logger zerolog.Logger, recorder ...PanicRecorder) func(http.Handler) http.Handler {
	var rec PanicRecorder = nopPanicRecorder{}
	if len(recorder) > 0 && recorder[0] != nil {
		rec = recorder[0]
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rv := recover(); rv != nil {
					rec.RecordPanic(r.URL.Path)
					logger.Error().
						Str("event", "panic_recovered").
						Str("request_id", IDFrom(r.Context())).
						Interface("panic", rv).
						Str("stack", string(debug.Stack())).
						Str("path", r.URL.Path).
						Str("method", r.Method).
						Msg("panic_recovered")
					http.Error(w, "internal server error", http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}
