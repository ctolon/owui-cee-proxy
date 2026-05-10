// Package tika implements the Engine adapter for Apache Tika Server.
//
// Tika's REST contract is "PUT raw bytes to /tika/text and read JSON
// back". The adapter accepts Docling-shaped multipart input, sends one
// PUT per file, then aggregates the responses into the Docling
// response shape so OpenWebUI can consume it without engine-specific
// branching.
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
	tikaTextPath = "/tika/text"
	versionPath  = "/version"
)

type Adapter struct {
	cfg     config.EngineConfig
	client  *httpclient.Client
	breaker *breaker.Breaker
}

func New(cfg config.EngineConfig, client *httpclient.Client, br *breaker.Breaker) (*Adapter, error) {
	if cfg.URL == "" {
		return nil, errors.New("tika: url is required")
	}
	return &Adapter{cfg: cfg, client: client, breaker: br}, nil
}

func (a *Adapter) Name() engine.Name { return engine.Tika }

// Convert implements engine.Engine. Tika has no /v1/convert/source
// equivalent, so HTTP sources are not supported.
func (a *Adapter) Convert(ctx context.Context, req *engine.ConvertRequest) (*engine.ConvertResponse, error) {
	if len(req.Sources) > 0 {
		return jsonError(http.StatusNotImplemented, "tika does not support http_sources; upload files instead"), nil
	}
	if len(req.Files) == 0 {
		return jsonError(http.StatusBadRequest, "no files in request"), nil
	}

	results := make([]doclingDocument, 0, len(req.Files))
	for _, f := range req.Files {
		doc, status, err := a.convertOne(ctx, f)
		if err != nil {
			return nil, err
		}
		if status >= 400 {
			return jsonError(status, fmt.Sprintf("tika upstream error: %d on %q", status, f.Filename)), nil
		}
		results = append(results, doc)
	}

	return wrapDoclingResponse(results)
}

func (a *Adapter) Health(ctx context.Context) error {
	p := a.cfg.HealthPath
	if p == "" {
		p = versionPath
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
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fmt.Errorf("tika health: status %d", resp.StatusCode)
	}
	return nil
}

func (a *Adapter) convertOne(ctx context.Context, f engine.FileBlob) (doclingDocument, int, error) {
	target, err := joinURL(a.cfg.URL, tikaTextPath)
	if err != nil {
		return doclingDocument{}, 0, err
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPut, target, f.Body)
	if err != nil {
		return doclingDocument{}, 0, err
	}
	if f.ContentType != "" {
		r.Header.Set("Content-Type", f.ContentType)
	}
	r.Header.Set("Accept", "application/json")
	if a.cfg.APIKey != "" {
		r.Header.Set("X-Api-Key", a.cfg.APIKey)
	}
	if v := a.cfg.ForwardOptions["x_tika_pdf_extract_inline_images"]; v == "true" {
		r.Header.Set("X-Tika-PDFextractInlineImages", "true")
	}

	resp, err := a.do(r)
	if err != nil {
		return doclingDocument{}, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return doclingDocument{}, resp.StatusCode, nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return doclingDocument{}, 0, err
	}

	doc, err := parseTikaResponse(body, f.Filename)
	if err != nil {
		return doclingDocument{}, 0, err
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

// jsonError returns a synthetic ConvertResponse carrying a Docling-shaped
// error envelope, so OpenWebUI can render it the same way it would
// render a real Docling failure.
func jsonError(status int, msg string) *engine.ConvertResponse {
	body, _ := json.Marshal(map[string]any{
		"status": "failure",
		"errors": []map[string]string{{"message": msg}},
	})
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
