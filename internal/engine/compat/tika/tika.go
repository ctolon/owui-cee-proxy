// Package tika implements the Engine adapter for Apache Tika Server.
//
// Tika's REST contract is "PUT raw bytes to /tika/text and read JSON
// back". The adapter accepts files in either facade (docling multipart
// or external raw PUT), sends one Tika PUT per file, and shapes the
// aggregated response back into the inbound facade's envelope —
// `document.md_content` for docling, `page_content` for external.
package tika

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

const (
	defaultTikaTextPath = "/tika/text"
	defaultHealthPath   = "/version"
)

type Adapter struct {
	name    engine.Name
	cfg     config.EngineConfig
	client  *httpclient.Client
	breaker *breaker.Breaker
}

func New(name engine.Name, cfg config.EngineConfig, client *httpclient.Client, br *breaker.Breaker) (*Adapter, error) {
	if cfg.URL == "" {
		return nil, errors.New("tika: url is required")
	}
	return &Adapter{name: name, cfg: cfg, client: client, breaker: br}, nil
}

func (a *Adapter) Name() engine.Name { return a.name }

func (a *Adapter) Capabilities() engine.EngineCapabilities {
	// Tika has no http_sources equivalent. Both facades are answered
	// because the adapter shapes the response per inbound facade.
	return engine.EngineCapabilities{
		Facades:     []engine.Facade{engine.FacadeDocling, engine.FacadeExternal},
		HTTPSources: false,
	}
}

func (a *Adapter) Convert(ctx context.Context, req *engine.ConvertRequest) (*engine.ConvertResponse, error) {
	if len(req.Sources) > 0 {
		return jsonError(req.Facade, http.StatusNotImplemented, "tika does not support http_sources; upload files instead"), nil
	}
	if len(req.Files) == 0 {
		return jsonError(req.Facade, http.StatusBadRequest, "no files in request"), nil
	}

	results := make([]item, 0, len(req.Files))
	for _, f := range req.Files {
		doc, status, err := a.convertOne(ctx, f)
		if err != nil {
			return nil, err
		}
		if status >= 400 {
			return jsonError(req.Facade, status, fmt.Sprintf("tika upstream error: %d on %q", status, f.Filename)), nil
		}
		results = append(results, doc)
	}

	switch req.Facade {
	case engine.FacadeExternal:
		return wrapExternalResponse(results)
	default:
		return wrapDoclingResponse(results)
	}
}

func (a *Adapter) Health(ctx context.Context) error {
	p := a.cfg.HealthPath
	if p == "" {
		p = defaultHealthPath
	}
	u, err := joinURL(a.cfg.URL, p)
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
		return fmt.Errorf("tika health: status %d", resp.StatusCode)
	}
	return nil
}

func (a *Adapter) tikaTextPath() string {
	if a.cfg.Paths.TikaText != "" {
		return a.cfg.Paths.TikaText
	}
	return defaultTikaTextPath
}

func (a *Adapter) convertOne(ctx context.Context, f engine.FileBlob) (item, int, error) {
	target, err := joinURL(a.cfg.URL, a.tikaTextPath())
	if err != nil {
		return item{}, 0, err
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPut, target, f.Body)
	if err != nil {
		return item{}, 0, err
	}
	if f.ContentType != "" {
		r.Header.Set("Content-Type", f.ContentType)
	}
	r.Header.Set("Accept", "application/json")
	if a.cfg.APIKey != "" && a.cfg.AuthHeader != "" {
		v := string(a.cfg.APIKey)
		if a.cfg.AuthScheme == "bearer" {
			v = "Bearer " + v
		}
		r.Header.Set(a.cfg.AuthHeader, v)
	}
	if v := a.cfg.ForwardOptions["x_tika_pdf_extract_inline_images"]; v == "true" {
		r.Header.Set("X-Tika-PDFextractInlineImages", "true")
	}

	resp, err := a.do(r)
	if err != nil {
		return item{}, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return item{}, resp.StatusCode, nil
	}

	doc, err := decodeTikaResponse(resp.Body, f.Filename)
	if err != nil {
		return item{}, 0, err
	}
	return doc, resp.StatusCode, nil
}

func (a *Adapter) do(r *http.Request) (*http.Response, error) {
	exec := func() (any, error) {
		return a.client.HTTP.Do(r)
	}
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
	return raw.(*http.Response), nil
}

// jsonError returns a synthetic ConvertResponse in whichever facade
// shape the caller expects.
func jsonError(facade engine.Facade, status int, msg string) *engine.ConvertResponse {
	var body []byte
	switch facade {
	case engine.FacadeExternal:
		body, _ = json.Marshal(map[string]any{
			"page_content": "",
			"metadata":     map[string]any{"error": msg},
		})
	default:
		body, _ = json.Marshal(map[string]any{
			"status": "failure",
			"errors": []map[string]string{{"message": msg}},
		})
	}
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	return &engine.ConvertResponse{
		StatusCode: status,
		Headers:    h,
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
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
