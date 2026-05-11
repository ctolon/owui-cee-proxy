package handlers

import (
	"context"

	"github.com/rs/zerolog"

	"github.com/ctolon/owui-cee-proxy/internal/engine"
	mw "github.com/ctolon/owui-cee-proxy/internal/transport/http/middleware"
)

// pickDecision is the structured payload of the `engine_pick_decision`
// debug record. Centralising the field set here eliminates the
// duplicate emit blocks in convert.go and external.go (C-32 from
// REVIEW-FAANG.md) — adding a new attribution field is now a
// single-file change.
type pickDecision struct {
	RequestID       string
	Engine          string
	EngineURL       string
	Facade          engine.Facade
	CompatType      string
	PickSource      engine.PickSource
	RoutingStrategy engine.RoutingStrategy
	MIMEDeclared    string
	MIMEResolved    string
	MIMESource      string
	Filename        string
	FileExt         string
	FileCount       int
}

// filepathExt extracts the lowercased last-suffix extension of a
// filename without dragging path/filepath into the import set.
// Empty for filenames with no dot or with a path separator after
// the last dot.
func filepathExt(name string) string {
	for i := len(name) - 1; i >= 0; i-- {
		switch name[i] {
		case '/', '\\':
			return ""
		case '.':
			ext := name[i:]
			// inline ASCII lowercase — same shape as
			// engine.filepathExt, kept local to avoid an engine
			// package re-export.
			b := []byte(ext)
			for j, c := range b {
				if c >= 'A' && c <= 'Z' {
					b[j] = c + 32
				}
			}
			return string(b)
		}
	}
	return ""
}

// logEnginePickDecision emits ONE structured debug record describing
// how the proxy resolved the inbound request to a specific engine.
// Skipped silently when the logger is above DebugLevel (so the
// production cost is one method call + a level compare).
//
// trace_id / span_id are pulled from ctx via mw.TraceFieldsFrom so
// the record joins cleanly against the AccessLog + any OTel spans
// the request is part of.
func logEnginePickDecision(ctx context.Context, logger zerolog.Logger, d pickDecision) {
	if logger.GetLevel() > zerolog.DebugLevel {
		return
	}
	traceID, spanID := mw.TraceFieldsFrom(ctx)
	logger.Debug().
		Str("event", "engine_pick_decision").
		Str("request_id", d.RequestID).
		Str("trace_id", traceID).
		Str("span_id", spanID).
		Str("engine", d.Engine).
		Str("engine_url", d.EngineURL).
		Str("facade", string(d.Facade)).
		Str("compat_type", d.CompatType).
		Str("pick_source", string(d.PickSource)).
		Str("routing_strategy", string(d.RoutingStrategy)).
		Str("mime_type", d.MIMEResolved). // legacy alias for back-compat
		Str("mime_declared", d.MIMEDeclared).
		Str("mime_resolved", d.MIMEResolved).
		Str("mime_source", d.MIMESource).
		Str("filename", d.Filename).
		Str("file_ext", d.FileExt).
		Int("file_count", d.FileCount).
		Msg("engine pick decision")
}
