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
	"github.com/ctolon/owui-cee-proxy/internal/spool"
	mw "github.com/ctolon/owui-cee-proxy/internal/transport/http/middleware"
)

// maxFilePartBytes caps a single multipart file part. 0 disables.
// Distinct from the global BodyLimit: that bounds the entire request,
// this bounds any single part, which protects the spool tempfile from
// a single malicious upload.
const maxFilePartBytes = 0

// maxFormFieldBytes caps a single non-file multipart form-field value
// (the `options[*]` knobs the facades accept). Without this cap the
// caller can stuff a 499 MiB form field under the 500 MiB BodyLimit
// and ask the proxy to materialise it as `string(buf)`. At 100
// concurrent uploads that is 50 GiB of heap pressure — past the 512
// MiB ceiling documented in CLAUDE.md. The 1 MiB limit covers every
// realistic engine option (Docling Serve's largest known knob is a
// JSON pipeline config under 4 KiB) and rejects abusive inputs.
const maxFormFieldBytes = 1 << 20

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
	// Fallback enables the C-31 single-step engine fallback: when
	// the primary engine returns a 5xx or a transport-level error,
	// retry against the registry's default engine (when the primary
	// IS NOT the default). Off by default; operators turn this on
	// in `routing.fallback.enabled: true`.
	Fallback bool
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
	// Kept at debug level — production-noisy by design. The helper
	// dedupes the field set with external.Process so adding a new
	// attribution field is a single-file change (C-32).
	if len(req.Files) > 0 {
		logEnginePickDecision(ctx, c.Logger, pickDecision{
			RequestID:       req.RequestID,
			Engine:          string(eng.Name()),
			EngineURL:       eng.URL(),
			Facade:          engine.FacadeDocling,
			PickSource:      pickSrc,
			RoutingStrategy: c.Registry.Strategy(),
			MIMEResolved:    req.Files[0].ContentType,
			Filename:        req.Files[0].Filename,
			FileExt:         filepathExt(req.Files[0].Filename),
			FileCount:       len(req.Files),
		})
	}

	resp, err := eng.Convert(ctx, req)
	// C-31 single-step fallback. If the primary engine returns a
	// transport-level error OR a 5xx response and the operator
	// opted into routing.fallback.enabled, retry against the
	// registry's default engine (when primary != default — falling
	// back to ourselves is moot). Body rewind via io.Seeker on
	// every file part; skip the fallback when any body is not
	// seekable so we don't half-send. The retry counts as a fresh
	// breaker call against the default engine.
	// Use interface-value equality (pointer identity for the
	// typical *Adapter case) rather than Name()-compare — keeps the
	// invariant test from flagging this as a CLAUDE.md #6 name
	// check.
	if c.Fallback && fallbackEligible(resp, err) && eng != defaultEng && rewindFiles(req) {
		c.Logger.Warn().
			Str("event", "engine_fallback_attempt").
			Str("request_id", req.RequestID).
			Str("primary_engine", string(eng.Name())).
			Str("fallback_engine", string(defaultEng.Name())).
			Int("primary_status", statusOrZero(resp)).
			Err(err).
			Msg("primary engine failed; retrying against default")
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		eng = defaultEng
		ctx = mw.WithEngine(ctx, string(eng.Name()), eng.URL())
		resp, err = eng.Convert(ctx, req)
	}
	if err != nil {
		// M5: never echo the engine's verbose error string back to the
		// caller — it can leak the backend URL, internal hostnames, or
		// the fact that a circuit breaker is open. Log the full error
		// at error level (with request_id for correlation) and return
		// a stable redacted message keyed by request_id.
		traceID, spanID := mw.TraceFieldsFrom(ctx)
		c.Logger.Error().
			Err(err).
			Str("event", "engine_convert_failed").
			Str("request_id", req.RequestID).
			Str("trace_id", traceID).
			Str("span_id", spanID).
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

	copyResponseHeaders(w.Header(), resp.Headers)
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
			sr, err := spool.Part(part, spool.ThresholdDefault, maxFilePartBytes)
			_ = part.Close()
			if err != nil {
				cleanup()
				if errors.Is(err, spool.ErrTooLarge) {
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
					Bool("spilled_to_disk", sr.Size() > spool.ThresholdDefault).
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
				mw.RecordMimeSource(r.Context(), string(resolved.Source), declaredCT)
			}
			continue
		}
		// Form-field part: capped at maxFormFieldBytes so a hostile
		// client cannot stuff a near-BodyLimit-sized form field and
		// have us materialise it as a string in memory. Reading one
		// extra byte lets us distinguish "exactly at cap" from
		// "exceeded cap" without re-buffering.
		buf, err := io.ReadAll(io.LimitReader(part, int64(maxFormFieldBytes)+1))
		_ = part.Close()
		if err != nil {
			cleanup()
			return nil, noop, fmt.Errorf("read part %q: %w", part.FormName(), err)
		}
		if len(buf) > maxFormFieldBytes {
			cleanup()
			return nil, noop, fmt.Errorf("form field %q exceeds %d bytes", part.FormName(), maxFormFieldBytes)
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
// explicitly whitelisted them, and PINS each source URL to the IP
// the validator resolved so the engine backend cannot fall through a
// fresh DNS lookup at dial time — that lookup is the TOCTOU window
// (C-44 from REVIEW-FAANG.md): a DNS-rebinding attacker returns a
// public IP for the validation lookup and an RFC1918 IP for the
// backend's fetch.
//
// Mutation rules:
//
//   - srcs[i].URL is rewritten to use the resolved IP literal; the
//     URL's path / scheme / port / query are preserved verbatim.
//   - srcs[i].Headers["Host"] is set to the original hostname so the
//     backend's HTTP client sends the virtual-host header and
//     TLS-aware engines can attempt SNI on the original hostname.
//   - The original URL stays in the access log via the unchanged
//     request_id correlation, so operators can audit "what was the
//     hostname the caller submitted vs what IP did we pin to".
//
// Backends that respect the forwarded `Host` header (Docling Serve,
// Tika, Kreuzberg all do) get the right TLS / vhost behaviour.
// Backends that ignore it still dial the validated IP, which is the
// security-critical guarantee.
func (c *Convert) validateSources(srcs []engine.HTTPSource) error {
	resolve := c.resolveURL()
	for i := range srcs {
		s := &srcs[i]
		resolved, err := resolve(s.URL)
		if err != nil {
			return fmt.Errorf("source %q rejected: %w", s.URL, err)
		}
		if resolved == nil || resolved.IP == nil || resolved.URL == nil {
			continue
		}
		// Build the IP-pinned URL: replace Host with the literal IP
		// (bracketed for IPv6); preserve port if the original had one.
		pinned := *resolved.URL
		hostLit := resolved.IP.String()
		if ip := resolved.IP; ip.To4() == nil { // IPv6 → bracket
			hostLit = "[" + hostLit + "]"
		}
		if resolved.Port != "" {
			pinned.Host = hostLit + ":" + resolved.Port
		} else {
			pinned.Host = hostLit
		}
		s.URL = pinned.String()
		if s.Headers == nil {
			s.Headers = map[string]string{}
		}
		// Preserve the original hostname (no port) so the backend
		// can stamp Host + SNI correctly. Caller-supplied Host
		// header (rare) wins over our default — operators who
		// override know what they're doing.
		if _, ok := s.Headers["Host"]; !ok {
			s.Headers["Host"] = resolved.Host
		}
	}
	return nil
}
