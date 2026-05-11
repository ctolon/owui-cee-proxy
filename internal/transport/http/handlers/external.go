package handlers

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/rs/zerolog"

	"github.com/ctolon/owui-cee-proxy/internal/config"
	"github.com/ctolon/owui-cee-proxy/internal/engine"
	"github.com/ctolon/owui-cee-proxy/internal/engine/compatutil"
	"github.com/ctolon/owui-cee-proxy/internal/engine/mimedetect"
	"github.com/ctolon/owui-cee-proxy/internal/engine/respond"
	mw "github.com/ctolon/owui-cee-proxy/internal/transport/http/middleware"
)

// External handles the OpenWebUI external loader facade
// (`PUT /process`).
//
// Inbound contract:
//   - Method: PUT
//   - Body:    raw bytes of the file
//   - Content-Type: caller's declared MIME (drives engine selection)
//   - X-Filename: URL-encoded filename hint (optional but useful)
//
// The handler builds an engine.ConvertRequest with Facade=External and
// dispatches via Registry.PickWithError(FacadeExternal, mime). The
// adapter's response is streamed back unchanged.
type External struct {
	Registry engine.Registry
	Logger   zerolog.Logger
	// MaxBytes caps the spooled body (0 disables; the global BodyLimit
	// middleware also enforces a request-wide cap).
	MaxBytes int64
	// UpstreamLog mirrors Convert.UpstreamLog — per-engine policy
	// resolver for non-2xx response logging.
	UpstreamLog func(engineName string) config.UpstreamLogConfig
	// Resolver: see Convert.Resolver. The composition root passes the
	// same instance to both handlers.
	Resolver *mimedetect.Resolver
}

func (e *External) Process(w http.ResponseWriter, r *http.Request) {
	// Baseline observability stamp: error paths from here on (missing
	// Content-Type, spool failure, no engine for facade) must NOT leave
	// the access log with empty engine + engine_url fields. Stamp the
	// default engine and routing strategy up front; later code
	// overwrites once dispatch settles.
	defaultEng := e.Registry.Default()
	r = r.WithContext(mw.WithEngine(r.Context(), string(defaultEng.Name()), defaultEng.URL()))
	r = r.WithContext(mw.WithPickDecision(r.Context(), string(engine.PickSourceDefault), string(e.Registry.Strategy())))

	declaredCT := r.Header.Get("Content-Type")
	if declaredCT == "" {
		writeJSON(w, http.StatusBadRequest, respond.NewExternalError("missing Content-Type"))
		return
	}

	filename := decodeFilename(r.Header.Get("X-Filename"))

	// Spool the request body to memory or disk based on size.
	sr, err := spoolPart(r.Body, int64(spoolThresholdDefault), e.MaxBytes)
	if err != nil {
		if errors.Is(err, ErrSpoolTooLarge) {
			writeJSON(w, http.StatusRequestEntityTooLarge, respond.NewExternalError("request body too large"))
			return
		}
		writeJSON(w, http.StatusBadRequest, respond.NewExternalError("read body failed"))
		return
	}
	defer func() { _ = sr.Close() }()

	// Resolve effective MIME — generic / empty declared types fall
	// back to a magic-byte sniff (and finally filename extension)
	// over the spool head, restored afterwards so the adapter still
	// streams the full body. Pinned down here BEFORE engine selection
	// so a client sending application/octet-stream + a PDF actually
	// routes to the PDF engine, not the default catch-all.
	resolved, srcerr := peekAndResolveMIME(declaredCT, filename, sr, e.Resolver)
	if srcerr != nil {
		writeJSON(w, http.StatusBadRequest, respond.NewExternalError("mime detect failed"))
		return
	}
	contentType := resolved.MIME

	req := &engine.ConvertRequest{
		Facade:    engine.FacadeExternal,
		Headers:   compatutil.AllowlistedHeaders(r.Header),
		RequestID: mw.IDFrom(r.Context()),
		Files: []engine.FileBlob{{
			Filename:    filename,
			ContentType: contentType,
			Size:        sr.Size(),
			Body:        sr,
		}},
	}

	eng, pickSrc, err := e.Registry.PickRouteVerbose(engine.FacadeExternal, engine.PickHint{
		MIME:     contentType,
		Filename: filename,
	})
	if err != nil {
		writeJSON(w, http.StatusNotImplemented,
			respond.NewExternalError(fmt.Sprintf("no engine for external facade: %v", err)))
		return
	}

	ctx := mw.WithEngine(r.Context(), string(eng.Name()), eng.URL())
	ctx = mw.WithFilename(ctx, filename)
	ctx = mw.WithFileCount(ctx, 1)
	ctx = mw.WithMimeType(ctx, contentType)
	ctx = mw.WithPickDecision(ctx, string(pickSrc), string(e.Registry.Strategy()))
	mw.RecordMimeSource(ctx, string(resolved.Source), declaredCT)

	traceID, spanID := mw.TraceFieldsFrom(ctx)
	if e.Logger.GetLevel() <= zerolog.DebugLevel {
		e.Logger.Debug().
			Str("event", "engine_pick_decision").
			Str("request_id", req.RequestID).
			Str("trace_id", traceID).
			Str("span_id", spanID).
			Str("engine", string(eng.Name())).
			Str("engine_url", eng.URL()).
			Str("pick_source", string(pickSrc)).
			Str("routing_strategy", string(e.Registry.Strategy())).
			Str("mime_type", contentType).
			Str("filename", filename).
			Msg("engine pick decision")
	}

	resp, err := eng.Convert(ctx, req)
	if err != nil {
		e.Logger.Error().
			Err(err).
			Str("event", "engine_convert_failed").
			Str("request_id", req.RequestID).
			Str("trace_id", traceID).
			Str("span_id", spanID).
			Str("engine", string(eng.Name())).
			Str("engine_url", eng.URL()).
			Msg("external facade engine convert failed")
		writeJSON(w, http.StatusBadGateway, respond.NewExternalError(
			fmt.Sprintf("engine %s failed (request_id=%s)", eng.Name(), req.RequestID)))
		return
	}
	if e.UpstreamLog != nil {
		resp = logUpstreamStatus(ctx, e.Logger, resp, e.UpstreamLog(string(eng.Name())),
			string(eng.Name()), eng.URL(), req.RequestID, filename)
	}
	defer func() { _ = resp.Body.Close() }()

	for k, v := range resp.Headers {
		for _, vv := range v {
			w.Header().Add(k, vv)
		}
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// decodeFilename URL-decodes an X-Filename header value and rejects
// any byte outside printable ASCII so it cannot poison logs or
// downstream multipart Content-Disposition.
func decodeFilename(raw string) string {
	if raw == "" {
		return "upload"
	}
	dec, err := url.QueryUnescape(raw)
	if err != nil {
		return "upload"
	}
	for i := 0; i < len(dec); i++ {
		b := dec[i]
		if b < 0x20 || b > 0x7E {
			return "upload"
		}
	}
	return dec
}
