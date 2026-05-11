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
}

func (e *External) Process(w http.ResponseWriter, r *http.Request) {
	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		http.Error(w, "missing Content-Type", http.StatusBadRequest)
		return
	}

	filename := decodeFilename(r.Header.Get("X-Filename"))

	// Spool the request body to memory or disk based on size.
	sr, err := spoolPart(r.Body, int64(spoolThresholdDefault), e.MaxBytes)
	if err != nil {
		if errors.Is(err, ErrSpoolTooLarge) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "read body failed", http.StatusBadRequest)
		return
	}
	defer func() { _ = sr.Close() }()

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

	eng, err := e.Registry.PickWithError(engine.FacadeExternal, contentType)
	if err != nil {
		http.Error(w, fmt.Sprintf("no engine for external facade: %v", err), http.StatusNotImplemented)
		return
	}

	ctx := mw.WithEngine(r.Context(), string(eng.Name()), eng.URL())
	ctx = mw.WithFilename(ctx, filename)
	ctx = mw.WithFileCount(ctx, 1)
	ctx = mw.WithMimeType(ctx, contentType)

	resp, err := eng.Convert(ctx, req)
	if err != nil {
		e.Logger.Error().
			Err(err).
			Str("event", "engine_convert_failed").
			Str("request_id", req.RequestID).
			Str("engine", string(eng.Name())).
			Str("engine_url", eng.URL()).
			Msg("external facade engine convert failed")
		http.Error(w, fmt.Sprintf("engine %s failed (request_id=%s)", eng.Name(), req.RequestID), http.StatusBadGateway)
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
