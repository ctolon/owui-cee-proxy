// Package docling implements the Engine adapter for the Docling Serve
// backend. Since the proxy facade is itself Docling-shaped, this adapter
// is essentially a streaming reverse proxy with a few tweaks: API-key
// header injection, request-id propagation, and response passthrough.
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
	"strings"

	"github.com/ctolon/owui-cee-proxy/internal/breaker"
	"github.com/ctolon/owui-cee-proxy/internal/config"
	"github.com/ctolon/owui-cee-proxy/internal/engine"
	"github.com/ctolon/owui-cee-proxy/internal/httpclient"
)

const (
	convertFilePath   = "/v1/convert/file"
	convertSourcePath = "/v1/convert/source"
)

type Adapter struct {
	cfg     config.EngineConfig
	client  *httpclient.Client
	breaker *breaker.Breaker
}

// New constructs a Docling adapter. Returns an error if cfg.URL is empty.
func New(cfg config.EngineConfig, client *httpclient.Client, br *breaker.Breaker) (*Adapter, error) {
	if cfg.URL == "" {
		return nil, errors.New("docling: url is required")
	}
	return &Adapter{cfg: cfg, client: client, breaker: br}, nil
}

func (a *Adapter) Name() engine.Name { return engine.Docling }

// Convert forwards the request to Docling. When the request carries
// HTTP sources, /v1/convert/source is used; otherwise files are sent
// to /v1/convert/file as multipart.
func (a *Adapter) Convert(ctx context.Context, req *engine.ConvertRequest) (*engine.ConvertResponse, error) {
	if len(req.Sources) > 0 {
		return a.convertSource(ctx, req)
	}
	return a.convertFile(ctx, req)
}

func (a *Adapter) Health(ctx context.Context) error {
	u, err := joinURL(a.cfg.URL, healthPath(a.cfg.HealthPath))
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
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fmt.Errorf("docling health: status %d", resp.StatusCode)
	}
	return nil
}

func (a *Adapter) convertFile(ctx context.Context, req *engine.ConvertRequest) (*engine.ConvertResponse, error) {
	target, err := joinURL(a.cfg.URL, convertFilePath)
	if err != nil {
		return nil, err
	}

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)

	// Write multipart in the background so the request body is streamed
	// rather than buffered in memory.
	go func() {
		defer pw.Close()
		defer mw.Close()
		for _, f := range req.Files {
			fw, werr := createFilePart(mw, "files", f.Filename, f.ContentType)
			if werr != nil {
				_ = pw.CloseWithError(werr)
				return
			}
			if _, werr := io.Copy(fw, f.Body); werr != nil {
				_ = pw.CloseWithError(werr)
				return
			}
		}
		// Forward facade options as form fields. Docling accepts most
		// ConvertDocumentsOptions fields as form fields.
		for k, v := range req.Options {
			if err := mw.WriteField(k, v); err != nil {
				_ = pw.CloseWithError(err)
				return
			}
		}
		// Layer in operator-configured forward_options last so they
		// override the user-supplied request.
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
	a.applyAuth(hreq.Header)
	a.applyForwardHeaders(hreq.Header, req.Headers)
	if req.RequestID != "" {
		hreq.Header.Set("X-Request-ID", req.RequestID)
	}

	return a.do(hreq)
}

func (a *Adapter) convertSource(ctx context.Context, req *engine.ConvertRequest) (*engine.ConvertResponse, error) {
	target, err := joinURL(a.cfg.URL, convertSourcePath)
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
	a.applyAuth(hreq.Header)
	a.applyForwardHeaders(hreq.Header, req.Headers)
	if req.RequestID != "" {
		hreq.Header.Set("X-Request-ID", req.RequestID)
	}
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

// maybeNormalize fixes the well-known Docling Serve quirk that returns
// "md_content": null (and siblings) when the conversion produced no
// extractable content. OpenWebUI 0.9.2's Pydantic loader rejects that
// with `Input should be a valid string`. Replacing null with "" keeps
// the response valid for strict consumers and is a no-op for permissive
// ones. The fix is byte-level so it does not pay the cost of a full
// JSON parse on multi-MB extractions.
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
		headers.Set("Content-Length", strconvItoa(len(patched)))
	}
	return io.NopCloser(bytes.NewReader(patched)), headers, nil
}

func isJSON(ct string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(ct)), "application/json")
}

// normalizeNullContent rewrites "<field>":null → "<field>":"" for the
// Docling content fields. JSON forbids the literal `null` inside string
// values, so this regex-free byte replace is safe.
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

// avoid pulling strconv into the call site; tiny inlined itoa.
func strconvItoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func (a *Adapter) applyAuth(h http.Header) {
	if a.cfg.APIKey != "" {
		h.Set("X-Api-Key", a.cfg.APIKey)
	}
}

// applyForwardHeaders copies a small allowlist of caller-supplied
// headers onto the outbound request. Hop-by-hop headers and auth
// headers are intentionally NOT forwarded — backend auth is the
// adapter's responsibility.
func (a *Adapter) applyForwardHeaders(dst, src http.Header) {
	allowlist := []string{"Accept", "Accept-Encoding", "User-Agent", "X-Request-ID"}
	for _, k := range allowlist {
		if v := src.Get(k); v != "" {
			dst.Set(k, v)
		}
	}
}

func createFilePart(mw *multipart.Writer, field, filename, contentType string) (io.Writer, error) {
	h := make(map[string][]string)
	disp := fmt.Sprintf(`form-data; name=%q; filename=%q`, field, escapeQuotes(filename))
	h["Content-Disposition"] = []string{disp}
	if contentType != "" {
		h["Content-Type"] = []string{contentType}
	}
	return mw.CreatePart(h)
}

func escapeQuotes(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s)
}

func joinURL(base, p string) (string, error) {
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
