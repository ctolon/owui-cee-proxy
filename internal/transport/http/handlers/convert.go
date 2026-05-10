// Package handlers contains the HTTP handlers for the Docling-compatible
// facade and the operational endpoints. The native passthrough mounts
// live in package passthrough but are wired together with the same
// middleware chain in server.go.
package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"

	"github.com/ctolon/owui-cee-proxy/internal/engine"
	mw "github.com/ctolon/owui-cee-proxy/internal/transport/http/middleware"
)

// Convert handles the Docling-compatible facade endpoints
// (/v1/convert/file and /v1/convert/source) by parsing the request,
// dispatching to the configured default engine, and streaming the
// adapter's response back to the caller.
type Convert struct {
	Registry engine.Registry
}

func (c *Convert) File(w http.ResponseWriter, r *http.Request) {
	c.handle(w, r, false)
}

func (c *Convert) Source(w http.ResponseWriter, r *http.Request) {
	c.handle(w, r, true)
}

func (c *Convert) handle(w http.ResponseWriter, r *http.Request, source bool) {
	req, cleanup, err := buildRequest(r, source)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"status": "failure",
			"errors": []map[string]string{{"message": err.Error()}},
		})
		return
	}
	defer cleanup()

	// MIME-based engine routing: the first file's Content-Type drives the
	// pick. Source-mode requests fall back to the default engine because
	// no MIME hint is available without dereferencing the URL.
	eng := c.Registry.Default()
	if !source && len(req.Files) > 0 {
		eng = c.Registry.Pick(req.Files[0].ContentType)
	}
	ctx := mw.WithEngine(r.Context(), string(eng.Name()), "")

	if len(req.Files) > 0 {
		ctx = mw.WithFilename(ctx, req.Files[0].Filename)
		ctx = mw.WithFileCount(ctx, len(req.Files))
	}
	req.RequestID = mw.IDFrom(ctx)

	resp, err := eng.Convert(ctx, req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"status": "failure",
			"errors": []map[string]string{{"message": fmt.Sprintf("engine %s: %v", eng.Name(), err)}},
		})
		return
	}
	defer resp.Body.Close()

	for k, v := range resp.Headers {
		for _, vv := range v {
			w.Header().Add(k, vv)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func buildRequest(r *http.Request, source bool) (*engine.ConvertRequest, func(), error) {
	if source {
		return buildSourceRequest(r)
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
		Headers: r.Header.Clone(),
		Options: map[string]string{},
	}
	// Parts are buffered into memory below, so no Closers to track.
	cleanup := func() {}

	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			cleanup()
			return nil, noop, fmt.Errorf("read part: %w", err)
		}
		// IMPORTANT: multipart.Reader drains a part when NextPart() is
		// invoked again, so we must read each part's body BEFORE moving
		// on. Otherwise the engine adapter receives empty file bodies.
		// The body cap is enforced by the BodyLimit middleware before
		// we even reach this function, so io.ReadAll is bounded.
		buf, err := io.ReadAll(part)
		_ = part.Close()
		if err != nil {
			cleanup()
			return nil, noop, fmt.Errorf("read part %q: %w", part.FormName(), err)
		}
		if part.FileName() != "" {
			req.Files = append(req.Files, engine.FileBlob{
				Filename:    part.FileName(),
				ContentType: part.Header.Get("Content-Type"),
				Size:        int64(len(buf)),
				Body:        bytes.NewReader(buf),
			})
			continue
		}
		req.Options[part.FormName()] = string(buf)
	}

	return req, cleanup, nil
}

type sourceBody struct {
	HTTPSources []engine.HTTPSource `json:"http_sources"`
	Options     map[string]string   `json:"options"`
}

func buildSourceRequest(r *http.Request) (*engine.ConvertRequest, func(), error) {
	var body sourceBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return nil, noop, fmt.Errorf("decode json: %w", err)
	}
	if len(body.HTTPSources) == 0 {
		return nil, noop, errors.New("http_sources required")
	}
	if err := validateSources(body.HTTPSources); err != nil {
		return nil, noop, err
	}
	return &engine.ConvertRequest{
		Sources: body.HTTPSources,
		Options: body.Options,
		Headers: r.Header.Clone(),
	}, noop, nil
}

func noop() {}

// validateSources blocks SSRF-prone targets unless the operator has
// explicitly whitelisted them. The default allowlist is "external
// only", i.e. not RFC1918, not loopback, not link-local, not the
// cloud metadata IP.
func validateSources(srcs []engine.HTTPSource) error {
	for _, s := range srcs {
		if err := isExternalURL(s.URL); err != nil {
			return fmt.Errorf("source %q rejected: %w", s.URL, err)
		}
	}
	return nil
}

// configurable hook to swap policy in tests.
var isExternalURL = defaultIsExternalURL

func _ctxValue(ctx context.Context, key any) any { return ctx.Value(key) }
