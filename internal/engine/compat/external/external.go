// Package external implements the Engine adapter for backends that
// speak OpenWebUI's "external loader" protocol:
//   - PUT {url}/process
//   - Body: raw bytes of the file
//   - Headers: Authorization: Bearer <key>, Content-Type: <mime>,
//     X-Filename: <url-encoded basename>
//   - Response: { "page_content": "...", "metadata": {...} } or list
//
// This adapter is reachable only via the external facade.
package external

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/ctolon/owui-cee-proxy/internal/breaker"
	"github.com/ctolon/owui-cee-proxy/internal/config"
	"github.com/ctolon/owui-cee-proxy/internal/engine"
	"github.com/ctolon/owui-cee-proxy/internal/httpclient"
)

const defaultProcessPath = "/process"

type Adapter struct {
	name    engine.Name
	cfg     config.EngineConfig
	client  *httpclient.Client
	breaker *breaker.Breaker
}

func New(name engine.Name, cfg config.EngineConfig, client *httpclient.Client, br *breaker.Breaker) (*Adapter, error) {
	if cfg.URL == "" {
		return nil, errors.New("external: url is required")
	}
	return &Adapter{name: name, cfg: cfg, client: client, breaker: br}, nil
}

func (a *Adapter) Name() engine.Name { return a.name }

func (a *Adapter) Capabilities() engine.EngineCapabilities {
	return engine.EngineCapabilities{
		Facades:     []engine.Facade{engine.FacadeExternal},
		HTTPSources: false,
	}
}

func (a *Adapter) Convert(ctx context.Context, req *engine.ConvertRequest) (*engine.ConvertResponse, error) {
	if len(req.Sources) > 0 {
		return jsonError(http.StatusNotImplemented, "external: http_sources not supported"), nil
	}
	if len(req.Files) == 0 {
		return jsonError(http.StatusBadRequest, "no files in request"), nil
	}
	// External loader is single-file by contract; proxy aggregates the
	// extracted text into a list shape when multiple files arrive.
	if len(req.Files) == 1 {
		return a.callOnce(ctx, req, req.Files[0])
	}
	return a.callBatched(ctx, req)
}

func (a *Adapter) Health(ctx context.Context) error {
	p := a.cfg.HealthPath
	if p == "" {
		p = "/health"
	}
	u, err := JoinURL(a.cfg.URL, p)
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
		return fmt.Errorf("external health: status %d", resp.StatusCode)
	}
	return nil
}

func (a *Adapter) processPath() string {
	if a.cfg.Paths.ExternalProcess != "" {
		return a.cfg.Paths.ExternalProcess
	}
	return defaultProcessPath
}

// callOnce streams a single file as the request body and forwards the
// upstream response unchanged when only one file is present.
func (a *Adapter) callOnce(ctx context.Context, req *engine.ConvertRequest, f engine.FileBlob) (*engine.ConvertResponse, error) {
	target, err := JoinURL(a.cfg.URL, a.processPath())
	if err != nil {
		return nil, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPut, target, f.Body)
	if err != nil {
		return nil, err
	}
	if f.ContentType != "" {
		hreq.Header.Set("Content-Type", f.ContentType)
	}
	hreq.Header.Set("X-Filename", url.PathEscape(SafeFilename(f.Filename)))
	ApplyAuth(hreq.Header, a.cfg)
	if req.RequestID != "" {
		hreq.Header.Set("X-Request-ID", req.RequestID)
	}
	return a.do(hreq)
}

// callBatched issues sequential PUT /process calls per file, then
// aggregates each response into a list. Each external loader response
// (single dict or list) is normalised to []externalDoc and concatenated.
func (a *Adapter) callBatched(ctx context.Context, req *engine.ConvertRequest) (*engine.ConvertResponse, error) {
	docs := make([]externalDoc, 0, len(req.Files))
	for _, f := range req.Files {
		one, err := a.callOnce(ctx, req, f)
		if err != nil {
			return nil, err
		}
		if one.StatusCode >= 400 {
			return one, nil
		}
		raw, rerr := io.ReadAll(one.Body)
		_ = one.Body.Close()
		if rerr != nil {
			return nil, rerr
		}
		parsed, perr := decodeExternalResponse(raw)
		if perr != nil {
			return nil, perr
		}
		docs = append(docs, parsed...)
	}
	out, err := json.Marshal(docs)
	if err != nil {
		return nil, err
	}
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	return &engine.ConvertResponse{
		StatusCode: http.StatusOK,
		Headers:    h,
		Body:       io.NopCloser(bytes.NewReader(out)),
	}, nil
}

func (a *Adapter) do(hreq *http.Request) (*engine.ConvertResponse, error) {
	exec := func() (any, error) { return a.client.HTTP.Do(hreq) }
	var raw any
	var err error
	if a.breaker != nil {
		raw, err = a.breaker.Execute(exec)
	} else {
		raw, err = exec()
	}
	if err != nil {
		return nil, err
	}
	resp := raw.(*http.Response)
	return &engine.ConvertResponse{
		StatusCode: resp.StatusCode,
		Headers:    resp.Header.Clone(),
		Body:       resp.Body,
	}, nil
}

// ApplyAuth stamps the auth header per the engine config.
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

// SafeFilename strips control bytes and any byte outside printable
// ASCII from a filename. Returns "" for empty / dangerous input.
func SafeFilename(name string) string {
	for i := 0; i < len(name); i++ {
		b := name[i]
		if b < 0x20 || b > 0x7E {
			return ""
		}
	}
	return name
}

// JoinURL composes a URL by appending p to base ensuring exactly one
// slash between them.
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

func jsonError(status int, msg string) *engine.ConvertResponse {
	body, _ := json.Marshal(map[string]any{
		"page_content": "",
		"metadata":     map[string]any{"error": msg},
	})
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	return &engine.ConvertResponse{
		StatusCode: status,
		Headers:    h,
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
}
