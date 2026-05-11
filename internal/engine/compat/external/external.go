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

	"github.com/ctolon/owui-cee-proxy/internal/breaker"
	"github.com/ctolon/owui-cee-proxy/internal/config"
	"github.com/ctolon/owui-cee-proxy/internal/engine"
	"github.com/ctolon/owui-cee-proxy/internal/engine/authutil"
	"github.com/ctolon/owui-cee-proxy/internal/engine/compatutil"
	"github.com/ctolon/owui-cee-proxy/internal/engine/respond"
	"github.com/ctolon/owui-cee-proxy/internal/httpclient"
	mw "github.com/ctolon/owui-cee-proxy/internal/transport/http/middleware"
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

func (a *Adapter) URL() string { return a.cfg.URL }

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
	authutil.ApplyMulti(hreq.Header, string(a.name), a.cfg, a.client.Auth())
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
	mw.StartUpstream(hreq.Context())
	if a.breaker != nil {
		raw, err = a.breaker.Execute(exec)
	} else {
		raw, err = exec()
	}
	if err != nil {
		mw.EndUpstream(hreq.Context(), 0)
		return nil, err
	}
	resp, ok := raw.(*http.Response)
	if !ok || resp == nil {
		mw.EndUpstream(hreq.Context(), 0)
		return nil, fmt.Errorf("external: breaker returned unexpected %T (want *http.Response)", raw)
	}
	mw.EndUpstream(hreq.Context(), resp.StatusCode)
	return &engine.ConvertResponse{
		StatusCode: resp.StatusCode,
		Headers:    resp.Header.Clone(),
		Body:       resp.Body,
	}, nil
}

// ApplyAuth is a thin shim around authutil.Apply for callers that
// don't have an engine name or AuthMetrics handle. Production call
// sites should prefer authutil.Apply directly so the outcome metric
// is recorded.
func ApplyAuth(h http.Header, cfg config.EngineConfig) {
	authutil.Apply(h, "", cfg, authutil.Nop{})
}

// SafeFilename and JoinURL forward to internal/engine/compatutil so the
// implementation lives in exactly one place. Kept exported because
// external_test.go and the doclingexternal composite reference them.
func SafeFilename(name string) string        { return compatutil.SafeFilename(name) }
func JoinURL(base, p string) (string, error) { return compatutil.JoinURL(base, p) }

func jsonError(status int, msg string) *engine.ConvertResponse {
	body := mustMarshalJSON(respond.NewExternalError(msg))
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	return &engine.ConvertResponse{
		StatusCode: status,
		Headers:    h,
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
}

// mustMarshalJSON panics on encoding errors for known-shape inputs.
// Marshaling typed structs from internal/engine/respond cannot fail
// at runtime; the panic guards against a future contract change.
func mustMarshalJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("external: encoder marshal of fixed shape failed: %v", err))
	}
	return b
}
