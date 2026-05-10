// Package kreuzberg implements the Engine adapter for the Kreuzberg
// extraction API server.
//
// Kreuzberg ships with a built-in OpenWebUI/Docling-compatible facade
// at POST /v1/convert/file (tag "openweb") that mirrors docling-serve's
// contract. We forward to that endpoint as a streaming reverse-proxy
// and do no response shaping: the project guarantees the returned
// envelope already contains `document.md_content` and `status`.
package kreuzberg

import (
	"bytes"
	"context"
	"encoding/json"
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

const convertFilePath = "/v1/convert/file"

type Adapter struct {
	cfg     config.EngineConfig
	client  *httpclient.Client
	breaker *breaker.Breaker
}

func New(cfg config.EngineConfig, client *httpclient.Client, br *breaker.Breaker) (*Adapter, error) {
	if cfg.URL == "" {
		return nil, errors.New("kreuzberg: url is required")
	}
	return &Adapter{cfg: cfg, client: client, breaker: br}, nil
}

func (a *Adapter) Name() engine.Name { return engine.Kreuzberg }

// Convert forwards the multipart upload to Kreuzberg's docling-serve
// compatible endpoint. HTTP source mode is not supported (Kreuzberg has
// no equivalent); we synthesise a Docling-shaped 501 envelope.
func (a *Adapter) Convert(ctx context.Context, req *engine.ConvertRequest) (*engine.ConvertResponse, error) {
	if len(req.Sources) > 0 {
		return jsonError(http.StatusNotImplemented, "kreuzberg does not support http_sources; upload files instead"), nil
	}
	if len(req.Files) == 0 {
		return jsonError(http.StatusBadRequest, "no files in request"), nil
	}

	target, err := joinURL(a.cfg.URL, convertFilePath)
	if err != nil {
		return nil, err
	}

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		defer func() { _ = pw.Close() }()
		defer func() { _ = mw.Close() }()
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
		// Forward operator-configured options as additional form fields.
		// Kreuzberg's docling-compat endpoint ignores unknown fields, so
		// passing extras is harmless.
		for k, v := range a.cfg.ForwardOptions {
			if werr := mw.WriteField(k, v); werr != nil {
				_ = pw.CloseWithError(werr)
				return
			}
		}
	}()

	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, target, pr)
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", mw.FormDataContentType())
	hreq.Header.Set("Accept", "application/json")
	if a.cfg.APIKey != "" {
		hreq.Header.Set("Authorization", "Bearer "+string(a.cfg.APIKey))
	}
	if req.RequestID != "" {
		hreq.Header.Set("X-Request-ID", req.RequestID)
	}

	resp, err := a.do(hreq)
	if err != nil {
		return nil, err
	}
	return &engine.ConvertResponse{
		StatusCode: resp.StatusCode,
		Headers:    resp.Header.Clone(),
		Body:       resp.Body,
	}, nil
}

func (a *Adapter) Health(ctx context.Context) error {
	p := a.cfg.HealthPath
	if p == "" {
		p = "/health"
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
		return fmt.Errorf("kreuzberg health: status %d", resp.StatusCode)
	}
	return nil
}

func (a *Adapter) do(r *http.Request) (*http.Response, error) {
	exec := func() (any, error) { return a.client.HTTP.Do(r) }
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

func createFilePart(mw *multipart.Writer, field, filename, contentType string) (io.Writer, error) {
	safe := safeMultipartFilename(filename)
	if safe == "" {
		safe = "upload"
	}
	h := make(map[string][]string)
	h["Content-Disposition"] = []string{
		fmt.Sprintf(`form-data; name=%q; filename=%q`, field, safe),
	}
	if contentType != "" {
		h["Content-Type"] = []string{contentType}
	}
	return mw.CreatePart(h)
}

// safeMultipartFilename sanitises a user-supplied filename for use in
// a multipart Content-Disposition header. CR/LF (or any byte outside
// printable ASCII) would let an attacker inject fake multipart headers
// (H5). We reject the input outright rather than try to escape it; a
// "" return signals the caller to substitute "upload".
func safeMultipartFilename(name string) string {
	for i := 0; i < len(name); i++ {
		b := name[i]
		if b < 0x20 || b > 0x7E {
			return ""
		}
	}
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(name)
}

// jsonError synthesises a Docling-shaped error envelope for cases the
// adapter rejects before it ever calls Kreuzberg.
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
