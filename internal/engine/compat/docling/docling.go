// Package docling implements the Engine adapter for backends that
// speak the Docling Serve protocol (POST /v1/convert/{file,source}).
// Configured engines with compat_type: docling — including Kreuzberg,
// since Kreuzberg's /v1/convert/file mirrors the Docling contract —
// are served by this single adapter.
//
// The adapter only answers the docling facade. For compat_type:
// docling-external (a backend that mirrors both endpoints) see the
// sibling composite package.
package docling

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"

	"github.com/ctolon/owui-cee-proxy/internal/breaker"
	"github.com/ctolon/owui-cee-proxy/internal/config"
	"github.com/ctolon/owui-cee-proxy/internal/engine"
	"github.com/ctolon/owui-cee-proxy/internal/engine/authutil"
	"github.com/ctolon/owui-cee-proxy/internal/engine/compatutil"
	"github.com/ctolon/owui-cee-proxy/internal/httpclient"
	mw "github.com/ctolon/owui-cee-proxy/internal/transport/http/middleware"
)

const (
	defaultConvertFilePath   = "/v1/convert/file"
	defaultConvertSourcePath = "/v1/convert/source"
)

type Adapter struct {
	name    engine.Name
	cfg     config.EngineConfig
	client  *httpclient.Client
	breaker *breaker.Breaker
}

// New constructs a docling-compat adapter. The name is the
// user-supplied engines.<name> key; it is what Name() returns.
func New(name engine.Name, cfg config.EngineConfig, client *httpclient.Client, br *breaker.Breaker) (*Adapter, error) {
	if cfg.URL == "" {
		return nil, errors.New("docling: url is required")
	}
	return &Adapter{name: name, cfg: cfg, client: client, breaker: br}, nil
}

func (a *Adapter) Name() engine.Name { return a.name }

func (a *Adapter) URL() string { return a.cfg.URL }

func (a *Adapter) Capabilities() engine.EngineCapabilities {
	return engine.EngineCapabilities{
		Facades:     []engine.Facade{engine.FacadeDocling},
		HTTPSources: true,
	}
}

// Convert routes by request shape. Files → /v1/convert/file (multipart);
// Sources → /v1/convert/source (JSON).
func (a *Adapter) Convert(ctx context.Context, req *engine.ConvertRequest) (*engine.ConvertResponse, error) {
	// Enforce the per-engine request_timeout. Without this wrap, the
	// configured timeout is silent dead code — only ResponseHeaderTimeout
	// fired, so a backend that hangs AFTER returning headers (e.g.,
	// streaming OCR output then deadlocking) holds the goroutine until
	// the server-wide chi Timeout middleware fires.
	ctx, cancel := a.client.WithDeadline(ctx)
	defer cancel()
	if len(req.Sources) > 0 {
		return a.convertSource(ctx, req)
	}
	return a.convertFile(ctx, req)
}

func (a *Adapter) Health(ctx context.Context) error {
	u, err := JoinURL(a.cfg.URL, healthPath(a.cfg.HealthPath))
	if err != nil {
		return err
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, u, http.NoBody)
	if err != nil {
		return err
	}
	resp, err := a.client.HTTP.Do(r)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 500 {
		return fmt.Errorf("docling health: status %d", resp.StatusCode)
	}
	return nil
}

func (a *Adapter) convertFilePath() string {
	if a.cfg.Paths.DoclingConvertFile != "" {
		return a.cfg.Paths.DoclingConvertFile
	}
	return defaultConvertFilePath
}

func (a *Adapter) convertSourcePath() string {
	if a.cfg.Paths.DoclingConvertSource != "" {
		return a.cfg.Paths.DoclingConvertSource
	}
	return defaultConvertSourcePath
}

func (a *Adapter) convertFile(ctx context.Context, req *engine.ConvertRequest) (*engine.ConvertResponse, error) {
	target, err := JoinURL(a.cfg.URL, a.convertFilePath())
	if err != nil {
		return nil, err
	}

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		defer func() { _ = pw.Close() }()
		defer func() { _ = mw.Close() }()
		for _, f := range req.Files {
			fw, werr := CreateFilePart(mw, "files", f.Filename, f.ContentType)
			if werr != nil {
				_ = pw.CloseWithError(werr)
				return
			}
			if _, werr := io.Copy(fw, f.Body); werr != nil {
				_ = pw.CloseWithError(werr)
				return
			}
		}
		for k, v := range req.Options {
			if err := mw.WriteField(k, v); err != nil {
				_ = pw.CloseWithError(err)
				return
			}
		}
		for k, v := range a.cfg.ForwardOptions {
			if err := mw.WriteField(k, v); err != nil {
				_ = pw.CloseWithError(err)
				return
			}
		}
	}()

	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, target, pr)
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", mw.FormDataContentType())
	authutil.ApplyMulti(hreq.Header, string(a.name), a.cfg, a.client.Auth())
	ApplyForwardHeaders(hreq.Header, req.Headers, req.RequestID)

	return a.do(hreq)
}

func (a *Adapter) convertSource(ctx context.Context, req *engine.ConvertRequest) (*engine.ConvertResponse, error) {
	target, err := JoinURL(a.cfg.URL, a.convertSourcePath())
	if err != nil {
		return nil, err
	}
	body, err := buildSourceJSON(req)
	if err != nil {
		return nil, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	authutil.ApplyMulti(hreq.Header, string(a.name), a.cfg, a.client.Auth())
	ApplyForwardHeaders(hreq.Header, req.Headers, req.RequestID)
	return a.do(hreq)
}

func (a *Adapter) do(hreq *http.Request) (*engine.ConvertResponse, error) {
	exec := func() (any, error) {
		resp, err := a.client.HTTP.Do(hreq)
		if err != nil {
			return nil, err
		}
		return resp, nil
	}
	var rawResp any
	var err error
	mw.StartUpstream(hreq.Context())
	if a.breaker != nil {
		rawResp, err = a.breaker.Execute(exec)
	} else {
		rawResp, err = exec()
	}
	if err != nil {
		mw.EndUpstream(hreq.Context(), 0)
		return nil, err
	}
	resp, ok := rawResp.(*http.Response)
	if !ok || resp == nil {
		mw.EndUpstream(hreq.Context(), 0)
		return nil, fmt.Errorf("docling: breaker returned unexpected %T (want *http.Response)", rawResp)
	}
	mw.EndUpstream(hreq.Context(), resp.StatusCode)
	body, headers, err := maybeNormalize(resp)
	if err != nil {
		_ = resp.Body.Close()
		return nil, err
	}
	return &engine.ConvertResponse{
		StatusCode: resp.StatusCode,
		Headers:    headers,
		Body:       body,
	}, nil
}

// maybeNormalize fixes the Docling Serve quirk that returns
// "md_content": null when conversion produced no extractable content.
// OpenWebUI's strict Pydantic loader rejects null for a string field;
// rewriting to "" keeps the response valid for strict consumers and is
// a no-op for permissive ones.
func maybeNormalize(resp *http.Response) (io.ReadCloser, http.Header, error) {
	headers := resp.Header.Clone()
	if !isJSON(resp.Header.Get("Content-Type")) {
		return resp.Body, headers, nil
	}
	raw, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, nil, err
	}
	patched := normalizeNullContent(raw)
	if len(patched) != len(raw) {
		headers.Set("Content-Length", strconv.Itoa(len(patched)))
	}
	return io.NopCloser(bytes.NewReader(patched)), headers, nil
}

func isJSON(ct string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(ct)), "application/json")
}

var nullableContentFields = []string{
	"md_content", "text_content", "html_content", "json_content", "doctags_content",
}

func normalizeNullContent(raw []byte) []byte {
	out := raw
	for _, field := range nullableContentFields {
		needle := []byte(`"` + field + `":null`)
		repl := []byte(`"` + field + `":""`)
		out = bytes.ReplaceAll(out, needle, repl)
	}
	return out
}

// ApplyAuth is a thin shim around authutil.Apply for callers that
// don't have an engine name or AuthMetrics handle (notably the
// doclingexternal composite, which used to live inside this package).
// Production call sites should prefer authutil.Apply directly so the
// outcome metric is recorded.
func ApplyAuth(h http.Header, cfg config.EngineConfig) {
	authutil.Apply(h, "", cfg, authutil.Nop{})
}

// ApplyForwardHeaders copies an allowlist of caller-supplied request
// headers onto the outbound request. X-Request-ID is always replaced
// with the server-generated value (H9).
func ApplyForwardHeaders(dst, src http.Header, requestID string) {
	for k := range fwdHeaderAllowlist {
		if v := src.Get(k); v != "" {
			dst.Set(k, v)
		}
	}
	dst.Del("X-Request-ID")
	if requestID != "" {
		dst.Set("X-Request-ID", requestID)
	}
}

var fwdHeaderAllowlist = map[string]struct{}{
	"Accept":          {},
	"Accept-Encoding": {},
	"User-Agent":      {},
}

// CreateFilePart writes a multipart file part with a sanitised
// filename. Exported so the docling-external composite can reuse it.
func CreateFilePart(mw *multipart.Writer, field, filename, contentType string) (io.Writer, error) {
	safe := SafeMultipartFilename(filename)
	if safe == "" {
		safe = "upload"
	}
	h := make(map[string][]string)
	disp := fmt.Sprintf(`form-data; name=%q; filename=%q`, field, safe)
	h["Content-Disposition"] = []string{disp}
	if contentType != "" {
		h["Content-Type"] = []string{contentType}
	}
	return mw.CreatePart(h)
}

// SafeMultipartFilename is a thin shim around compatutil so sibling
// adapters (doclingexternal composite) still find the symbol where it
// historically lived. Prefer compatutil.SafeMultipartFilename in new
// code.
func SafeMultipartFilename(name string) string {
	return compatutil.SafeMultipartFilename(name)
}

// JoinURL is a thin shim around compatutil.JoinURL kept for sibling
// adapters that imported docling.JoinURL when this package owned the
// canonical copy.
func JoinURL(base, p string) (string, error) {
	return compatutil.JoinURL(base, p)
}

func healthPath(p string) string {
	if p == "" {
		return "/health"
	}
	return p
}
