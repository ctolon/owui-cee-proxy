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
}

func (c *Convert) File(w http.ResponseWriter, r *http.Request) {
	c.handle(w, r, false)
}

func (c *Convert) Source(w http.ResponseWriter, r *http.Request) {
	c.handle(w, r, true)
}

func (c *Convert) handle(w http.ResponseWriter, r *http.Request, source bool) {
	req, cleanup, err := c.buildRequest(r, source)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, respond.NewDoclingError(err.Error()))
		return
	}
	defer cleanup()

	// MIME-based engine routing: the first file's Content-Type drives the
	// pick. Source-mode requests fall back to the default engine because
	// no MIME hint is available without dereferencing the URL.
	//
	// M13: this fallback is INTENTIONAL — source mode skips MIME-based
	// dispatch and always uses the default engine. Operators rely on the
	// log line below to debug "why didn't my Tika rule fire?".
	req.Facade = engine.FacadeDocling
	eng := c.Registry.Default()
	if !source && len(req.Files) > 0 {
		eng = c.Registry.Pick(engine.FacadeDocling, req.Files[0].ContentType)
	} else if source {
		c.Logger.Info().
			Str("event", "source_mode_default_engine").
			Str("request_id", mw.IDFrom(r.Context())).
			Str("engine", string(eng.Name())).
			Msg("source mode invoked; routing to default engine (MIME-based dispatch unavailable)")
	}
	ctx := mw.WithEngine(r.Context(), string(eng.Name()), eng.URL())

	if len(req.Files) > 0 {
		ctx = mw.WithFilename(ctx, req.Files[0].Filename)
		ctx = mw.WithFileCount(ctx, len(req.Files))
		ctx = mw.WithMimeType(ctx, req.Files[0].ContentType)
	}
	req.RequestID = mw.IDFrom(ctx)

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
	return buildFileRequest(r)
}

func buildFileRequest(r *http.Request) (*engine.ConvertRequest, func(), error) {
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
			// back to magic-byte sniff when client sent
			// application/octet-stream or empty. Fixes the user-reported
			// "default engine catches PDFs because Content-Type is
			// generic" routing bug.
			resolved, srcerr := peekAndResolveMIME(declaredCT, sr)
			if srcerr != nil {
				cleanup()
				return nil, noop, fmt.Errorf("mime detect %q: %w", part.FileName(), srcerr)
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
