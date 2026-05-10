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
	"net/url"
	"strconv"
	"strings"

	"github.com/ctolon/owui-cee-proxy/internal/breaker"
	"github.com/ctolon/owui-cee-proxy/internal/config"
	"github.com/ctolon/owui-cee-proxy/internal/engine"
	"github.com/ctolon/owui-cee-proxy/internal/httpclient"
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

func (a *Adapter) Capabilities() engine.EngineCapabilities {
	return engine.EngineCapabilities{
		Facades:     []engine.Facade{engine.FacadeDocling},
		HTTPSources: true,
	}
}

// Convert routes by request shape. Files → /v1/convert/file (multipart);
// Sources → /v1/convert/source (JSON).
func (a *Adapter) Convert(ctx context.Context, req *engine.ConvertRequest) (*engine.ConvertResponse, error) {
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
	ApplyAuth(hreq.Header, a.cfg)
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
	ApplyAuth(hreq.Header, a.cfg)
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
	if a.breaker != nil {
		rawResp, err = a.breaker.Execute(exec)
	} else {
		rawResp, err = exec()
	}
	if err != nil {
		return nil, err
	}
	resp, _ := rawResp.(*http.Response)
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

// ApplyAuth stamps the configured auth header onto the outbound
// request. Exported so the docling-external composite can reuse it.
func ApplyAuth(h http.Header, cfg config.EngineConfig) {
	if cfg.APIKey == "" || cfg.AuthHeader == "" {
		return
	}
	value := string(cfg.APIKey)
	if cfg.AuthScheme == "bearer" {
		value = "Bearer " + value
	}
	h.Set(cfg.AuthHeader, value)
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

// SafeMultipartFilename sanitises a user-supplied filename. Returns ""
// for inputs containing CR/LF/NUL or any byte outside printable ASCII
// (H5). Caller substitutes a placeholder ("upload") in that case.
func SafeMultipartFilename(name string) string {
	for i := 0; i < len(name); i++ {
		b := name[i]
		if b < 0x20 || b > 0x7E {
			return ""
		}
	}
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(name)
}

// JoinURL appends p to base, ensuring a single slash between them.
// Exported so sibling adapters reuse the same URL composition.
func JoinURL(base, p string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("invalid url: %w", err)
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	u.Path = strings.TrimRight(u.Path, "/") + p
	return u.String(), nil
}

func healthPath(p string) string {
	if p == "" {
		return "/health"
	}
	return p
}
