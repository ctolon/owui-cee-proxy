// Package handlers contains the HTTP handlers for the Docling-compatible
// facade and the operational endpoints. The native passthrough mounts
// live in package passthrough but are wired together with the same
// middleware chain in server.go.
package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"

	"github.com/rs/zerolog"

	"github.com/ctolon/owui-cee-proxy/internal/config"
	"github.com/ctolon/owui-cee-proxy/internal/engine"
	"github.com/ctolon/owui-cee-proxy/internal/engine/compatutil"
	"github.com/ctolon/owui-cee-proxy/internal/engine/mimedetect"
	"github.com/ctolon/owui-cee-proxy/internal/engine/respond"
	mw "github.com/ctolon/owui-cee-proxy/internal/transport/http/middleware"
)

// maxFilePartBytes caps a single multipart file part. 0 disables.
// Distinct from the global BodyLimit: that bounds the entire request,
// this bounds any single part, which protects the spool tempfile from
// a single malicious upload.
const maxFilePartBytes = 0

// Convert handles the Docling-compatible facade endpoints
// (/v1/convert/file and /v1/convert/source) by parsing the request,
// dispatching to the configured default engine, and streaming the
// adapter's response back to the caller.
//
// ResolveURL implements the SSRF policy for /v1/convert/source. It is
// a struct field rather than a package-level var so tests (and future
// per-instance overrides) don't have to mutate global state. Nil
// falls back to the default policy (defaultResolveExternalURL).
type Convert struct {
	Registry   engine.Registry
	Logger     zerolog.Logger
	ResolveURL func(raw string) (*ResolvedURL, error)
	// UpstreamLog resolves the effective upstream-log policy for a
	// given engine name. Nil = upstream-error logging disabled, which
	// matches v0.1.x behaviour and keeps existing tests green.
	UpstreamLog func(engineName string) config.UpstreamLogConfig
	// Resolver is the per-process MIME resolver carrying the merged
	// (built-in + operator override) extension map. Required — the
	// composition root constructs it from cfg.Mimedetect and injects
	// here. Nil is treated as a misconfiguration; the handler panics
	// at first request to fail loud rather than silently degrade to
	// the no-overrides default.
	Resolver *mimedetect.Resolver
}

func (c *Convert) File(w http.ResponseWriter, r *http.Request) {
	c.handle(w, r, false)
}

func (c *Convert) Source(w http.ResponseWriter, r *http.Request) {
	c.handle(w, r, true)
}

func (c *Convert) handle(w http.ResponseWriter, r *http.Request, source bool) {
	// Baseline observability stamp: even if buildRequest fails (malformed
	// multipart, bad JSON, ...) we want the access-log line to carry
	// non-empty engine + engine_url + routing_strategy. The default
	// engine is the catch-all; later code overwrites these fields if a
	// different engine wins dispatch. This fixes the reported "engine
	// and engine_url come back empty in routing" symptom on 4xx exit
	// paths.
	defaultEng := c.Registry.Default()
	r = r.WithContext(mw.WithEngine(r.Context(), string(defaultEng.Name()), defaultEng.URL()))
	r = r.WithContext(mw.WithPickDecision(r.Context(), string(engine.PickSourceDefault), string(c.Registry.Strategy())))

	req, cleanup, err := c.buildRequest(r, source)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, respond.NewDoclingError(err.Error()))
		return
	}
	defer cleanup()

	// Engine routing: routing.strategy controls whether dispatch uses
	// the MIME allowlist, the extension allowlist, or both phases in
	// sequence. The FIRST file's MIME + filename drive the pick.
	// Source-mode requests have no upload signal, so they always fall
	// through to PickSourceDefault — that's by design (M13).
	req.Facade = engine.FacadeDocling
	eng := defaultEng
	pickSrc := engine.PickSourceDefault
	if !source && len(req.Files) > 0 {
		picked, src, perr := c.Registry.PickRouteVerbose(engine.FacadeDocling, engine.PickHint{
			MIME:     req.Files[0].ContentType,
			Filename: req.Files[0].Filename,
		})
		if perr == nil {
			eng = picked
			pickSrc = src
		}
	} else if source {
		c.Logger.Info().
			Str("event", "source_mode_default_engine").
			Str("request_id", mw.IDFrom(r.Context())).
			Str("engine", string(eng.Name())).
			Msg("source mode invoked; routing to default engine (MIME-based dispatch unavailable)")
	}
	ctx := mw.WithEngine(r.Context(), string(eng.Name()), eng.URL())
	ctx = mw.WithPickDecision(ctx, string(pickSrc), string(c.Registry.Strategy()))

	if len(req.Files) > 0 {
		ctx = mw.WithFilename(ctx, req.Files[0].Filename)
		ctx = mw.WithFileCount(ctx, len(req.Files))
		ctx = mw.WithMimeType(ctx, req.Files[0].ContentType)
	}
	req.RequestID = mw.IDFrom(ctx)

	// One-shot debug record so operators can audit per-request routing
	// without joining the access-log line against a separate trace.
	// Kept at debug level — production-noisy by design.
	if c.Logger.GetLevel() <= zerolog.DebugLevel && len(req.Files) > 0 {
		c.Logger.Debug().
			Str("event", "engine_pick_decision").
			Str("request_id", req.RequestID).
			Str("engine", string(eng.Name())).
			Str("engine_url", eng.URL()).
			Str("pick_source", string(pickSrc)).
			Str("routing_strategy", string(c.Registry.Strategy())).
			Str("mime_type", req.Files[0].ContentType).
			Str("filename", req.Files[0].Filename).
			Msg("engine pick decision")
	}

	resp, err := eng.Convert(ctx, req)
	if err != nil {
		// M5: never echo the engine's verbose error string back to the
		// caller — it can leak the backend URL, internal hostnames, or
		// the fact that a circuit breaker is open. Log the full error
		// at error level (with request_id for correlation) and return
		// a stable redacted message keyed by request_id.
		c.Logger.Error().
			Err(err).
			Str("event", "engine_convert_failed").
			Str("request_id", req.RequestID).
			Str("engine", string(eng.Name())).
			Str("engine_url", eng.URL()).
			Msg("engine convert failed")
		writeJSON(
			w, http.StatusBadGateway,
			respond.NewDoclingError(fmt.Sprintf("engine %s failed (request_id=%s)", eng.Name(), req.RequestID)),
		)
		return
	}
	// Surface non-2xx engine responses to the operator log. The helper
	// also peek-restores the body so io.Copy below still sees the full
	// upstream payload.
	if c.UpstreamLog != nil {
		resp = logUpstreamStatus(ctx, c.Logger, resp, c.UpstreamLog(string(eng.Name())),
			string(eng.Name()), eng.URL(), req.RequestID, currentFilename(req))
	}
	defer func() { _ = resp.Body.Close() }()

	for k, v := range resp.Headers {
		for _, vv := range v {
			w.Header().Add(k, vv)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func currentFilename(req *engine.ConvertRequest) string {
	if len(req.Files) > 0 {
		return req.Files[0].Filename
	}
	return ""
}

func (c *Convert) buildRequest(r *http.Request, source bool) (*engine.ConvertRequest, func(), error) {
	if source {
		return c.buildSourceRequest(r)
	}
	return c.buildFileRequest(r)
}

func (c *Convert) buildFileRequest(r *http.Request) (*engine.ConvertRequest, func(), error) {
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return nil, noop, fmt.Errorf("parse content-type: %w", err)
	}
	if !strings.HasPrefix(mediaType, "multipart/") {
		return nil, noop, errors.New("content-type must be multipart/form-data")
	}
	mr := multipart.NewReader(r.Body, params["boundary"])

	req := &engine.ConvertRequest{
		Headers: compatutil.AllowlistedHeaders(r.Header),
		Options: map[string]string{},
	}
	// closers tracks every spool reader / temp file that the cleanup
	// callback must release after the engine response is written.
	var closers []io.Closer
	cleanup := func() {
		for _, c := range closers {
			_ = c.Close()
		}
	}

	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			cleanup()
			return nil, noop, fmt.Errorf("read part: %w", err)
		}
		if part.FileName() != "" {
			// File part: spool to memory (small) or disk (large). The
			// global BodyLimit middleware already bounds the request
			// stream, so this only adds the per-part overflow guard.
			declaredCT := part.Header.Get("Content-Type")
			sr, err := spoolPart(part, spoolThresholdDefault, maxFilePartBytes)
			_ = part.Close()
			if err != nil {
				cleanup()
				if errors.Is(err, ErrSpoolTooLarge) {
					return nil, noop, fmt.Errorf("file %q too large", part.FileName())
				}
				return nil, noop, fmt.Errorf("read part %q: %w", part.FormName(), err)
			}
			closers = append(closers, sr)

			// Resolve effective MIME: declared wins when specific; fall
			// back to magic-byte sniff (and finally filename extension)
			// when client sent application/octet-stream or empty. Fixes
			// the user-reported "default engine catches PDFs because
			// Content-Type is generic" routing bug AND the .msg CFB
			// disambiguation bug — both flow through the same Resolver.
			resolved, srcerr := peekAndResolveMIME(declaredCT, part.FileName(), sr, c.Resolver)
			if srcerr != nil {
				cleanup()
				return nil, noop, fmt.Errorf("mime detect %q: %w", part.FileName(), srcerr)
			}

			// Per-part debug breadcrumb. Operators reading a debug
			// log timeline want to know exactly when each upload
			// piece arrived, how big it was, whether it spilled to
			// disk, and how the MIME got resolved. All in one event.
			if c.Logger.GetLevel() <= zerolog.DebugLevel {
				c.Logger.Debug().
					Str("event", "multipart_part_received").
					Str("request_id", mw.IDFrom(r.Context())).
					Str("filename", part.FileName()).
					Str("declared_content_type", declaredCT).
					Str("resolved_mime", resolved.MIME).
					Str("mime_source", string(resolved.Source)).
					Int64("size_bytes", sr.Size()).
					Bool("spilled_to_disk", sr.Size() > spoolThresholdDefault).
					Msg("multipart part received")
			}

			req.Files = append(req.Files, engine.FileBlob{
				Filename:    part.FileName(),
				ContentType: resolved.MIME,
				Size:        sr.Size(),
				Body:        sr,
			})
			// Record the source for the FIRST file only — that's the
			// file MIME-based routing uses; per-file source logs would
			// be noise. The middleware reads this back via
			// mw.MIMESourceFrom on the access-log emission path.
			if len(req.Files) == 1 {
				mw.WithMimeSource(r.Context(), string(resolved.Source), declaredCT)
			}
			continue
		}
		// Form-field part: assume small, read into memory.
		buf, err := io.ReadAll(part)
		_ = part.Close()
		if err != nil {
			cleanup()
			return nil, noop, fmt.Errorf("read part %q: %w", part.FormName(), err)
		}
		req.Options[part.FormName()] = string(buf)
	}

	return req, cleanup, nil
}

type sourceBody struct {
	HTTPSources []engine.HTTPSource `json:"http_sources"`
	Options     map[string]string   `json:"options"`
}

func (c *Convert) buildSourceRequest(r *http.Request) (*engine.ConvertRequest, func(), error) {
	var body sourceBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return nil, noop, fmt.Errorf("decode json: %w", err)
	}
	if len(body.HTTPSources) == 0 {
		return nil, noop, errors.New("http_sources required")
	}
	if err := c.validateSources(body.HTTPSources); err != nil {
		return nil, noop, err
	}
	return &engine.ConvertRequest{
		Sources: body.HTTPSources,
		Options: body.Options,
		Headers: compatutil.AllowlistedHeaders(r.Header),
	}, noop, nil
}

func noop() {}

// resolveURL returns the configured resolver, defaulting to the
// package's standard policy when c.ResolveURL is nil. L3.
func (c *Convert) resolveURL() func(string) (*ResolvedURL, error) {
	if c.ResolveURL != nil {
		return c.ResolveURL
	}
	return defaultResolveExternalURL
}

// validateSources blocks SSRF-prone targets unless the operator has
// explicitly whitelisted them. The default allowlist is "external
// only", i.e. not RFC1918, not loopback, not link-local, not the
// cloud metadata IP. The resolved IP is recorded on the source URL
// (as part of validation) but the actual outbound dial happens later
// inside the engine adapter; B1's fix ships in a follow-up wave that
// threads the *http.Transport through this layer.
func (c *Convert) validateSources(srcs []engine.HTTPSource) error {
	resolve := c.resolveURL()
	for _, s := range srcs {
		if _, err := resolve(s.URL); err != nil {
			return fmt.Errorf("source %q rejected: %w", s.URL, err)
		}
	}
	return nil
}
