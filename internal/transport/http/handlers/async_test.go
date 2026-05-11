package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ctolon/owui-cee-proxy/internal/config"
	"github.com/ctolon/owui-cee-proxy/internal/engine"
	"github.com/ctolon/owui-cee-proxy/internal/tasks"
)

// stubEngine is a minimal Engine usable as a registry default. It is
// never actually invoked in these tests because the request is rejected
// or short-circuited before any engine.Convert call is made.
type stubEngine struct {
	name        engine.Name
	httpSources bool
	// facades opts an instance into a specific set of facades. Empty
	// keeps the historical docling-only default so existing tests
	// don't shift behaviour.
	facades []engine.Facade
}

func (s *stubEngine) Name() engine.Name { return s.name }
func (s *stubEngine) URL() string       { return "http://" + string(s.name) + ".test" }
func (s *stubEngine) Capabilities() engine.EngineCapabilities {
	fs := s.facades
	if len(fs) == 0 {
		fs = []engine.Facade{engine.FacadeDocling}
	}
	return engine.EngineCapabilities{
		Facades:     fs,
		HTTPSources: s.httpSources,
	}
}

func (s *stubEngine) Convert(_ context.Context, _ *engine.ConvertRequest) (*engine.ConvertResponse, error) {
	return nil, nil
}

func (s *stubEngine) Health(_ context.Context) error { return nil }

func newRegistry(t *testing.T, def engine.Name, names ...engine.Name) engine.Registry {
	t.Helper()
	entries := map[engine.Name]engine.RegistryEntry{
		def: {Engine: &stubEngine{name: def}},
	}
	for _, n := range names {
		entries[n] = engine.RegistryEntry{Engine: &stubEngine{name: n}}
	}
	reg, err := engine.NewRegistry(entries, def, "")
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	return reg
}

// newOrchestratorOrSkip constructs an Orchestrator pointed at a Redis
// URL that will never be reached in the test (the code path under test
// rejects before any Redis call). If construction fails for an
// unrelated reason we skip rather than fail.
func newOrchestratorOrSkip(t *testing.T) *tasks.Orchestrator {
	t.Helper()
	cfg := config.TasksConfig{
		Enabled:       true,
		RedisURL:      "redis://127.0.0.1:65535/0",
		Retention:     1,
		ResultTTL:     1,
		MaxBlobBytes:  1 << 20,
		MaxTotalBytes: 4 << 20,
		TaskTimeout:   1,
	}
	orch, err := tasks.NewOrchestrator(cfg, nil)
	if err != nil {
		t.Skipf("orchestrator construction failed: %v", err)
	}
	t.Cleanup(func() { _ = orch.Close() })
	return orch
}

func TestAsync_SubmitSource_RejectsEngineWithoutHTTPSources(t *testing.T) {
	t.Parallel()

	// Default engine has Capabilities().HTTPSources=false (the stub
	// default) — so http_sources requests should be rejected with 400.
	reg := newRegistry(t, "default-no-sources", "secondary")
	orch := newOrchestratorOrSkip(t)

	a := &Async{Registry: reg, Orchestrator: orch}

	body := bytes.NewBufferString(`{"http_sources":[{"url":"https://example.com/x.pdf"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/convert/source/async", body)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	a.SubmitSource(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "http_sources") {
		t.Fatalf("expected error mentioning http_sources, got %q", rr.Body.String())
	}
}

func TestAsync_SubmitFile_TokenShape_OnSuccess(t *testing.T) {
	t.Parallel()
	// Without a real Redis we cannot exercise a successful Enqueue.
	// Instead validate the response shape via direct token minting
	// helper checks — left as a probe so a follow-up integration
	// test can assert the wire shape.
	t.Skip("requires running Redis; covered by integration tests")
}

func TestWriteResultResponse_DisallowedCTForcedDownload(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name           string
		ct             string
		wantCT         string
		wantDispPrefix string
	}{
		{"image-png-forced-attachment", "image/png", "application/octet-stream", "attachment;"},
		{"binary-forced-attachment", "application/zip", "application/octet-stream", "attachment;"},
		{"json-passes-through", "application/json", "application/json", ""},
		{"text-html-passes-through", "text/html; charset=utf-8", "text/html; charset=utf-8", ""},
		{"empty-defaults-to-json", "", "application/json", ""},
		{"unknown-script-forced", "application/javascript", "application/octet-stream", "attachment;"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rr := httptest.NewRecorder()
			writeResultResponse(rr, strings.NewReader("payload"), tc.ct)
			if got := rr.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Fatalf("expected nosniff header, got %q", got)
			}
			if got := rr.Header().Get("Content-Type"); got != tc.wantCT {
				t.Fatalf("Content-Type: got %q want %q", got, tc.wantCT)
			}
			disp := rr.Header().Get("Content-Disposition")
			if tc.wantDispPrefix == "" && disp != "" {
				t.Fatalf("did not expect Content-Disposition, got %q", disp)
			}
			if tc.wantDispPrefix != "" && !strings.HasPrefix(disp, tc.wantDispPrefix) {
				t.Fatalf("Content-Disposition: got %q want prefix %q", disp, tc.wantDispPrefix)
			}
		})
	}
}

// newExternalRegistry wires a single external-facade engine as the
// default. Used by the SubmitProcess tests so the External facade
// pick path resolves an engine.
func newExternalRegistry(t *testing.T) engine.Registry {
	t.Helper()
	entries := map[engine.Name]engine.RegistryEntry{
		"ext-default": {Engine: &stubEngine{
			name:    "ext-default",
			facades: []engine.Facade{engine.FacadeExternal},
		}},
	}
	reg, err := engine.NewRegistry(entries, "ext-default", "")
	if err != nil {
		t.Fatalf("external registry: %v", err)
	}
	return reg
}

func TestAsync_SubmitProcess_RejectsMissingContentType(t *testing.T) {
	t.Parallel()
	reg := newExternalRegistry(t)
	orch := newOrchestratorOrSkip(t)
	a := &Async{Registry: reg, Orchestrator: orch}

	req := httptest.NewRequest(http.MethodPost, "/process/async", strings.NewReader("hello"))
	// deliberately do NOT set Content-Type
	rr := httptest.NewRecorder()
	a.SubmitProcess(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status: want 400 got %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "Content-Type") {
		t.Fatalf("expected error mentioning Content-Type, got %q", rr.Body.String())
	}
}

func TestAsync_SubmitProcess_RejectsWhenOrchestratorMissing(t *testing.T) {
	t.Parallel()
	reg := newExternalRegistry(t)
	// Orchestrator left nil — the handler must short-circuit with
	// 501 (matching the docling-facade async behaviour).
	a := &Async{Registry: reg}

	req := httptest.NewRequest(http.MethodPost, "/process/async", strings.NewReader("hello"))
	req.Header.Set("Content-Type", "text/plain")
	rr := httptest.NewRecorder()
	a.SubmitProcess(rr, req)

	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("status: want 501 got %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "async tasks not enabled") {
		t.Fatalf("expected friendly error, got %q", rr.Body.String())
	}
}

func TestAsync_SubmitProcess_RejectsWhenNoEngineForFacade(t *testing.T) {
	t.Parallel()
	// Registry whose only engine speaks the docling facade, NOT
	// external. PickRoute on the external facade must fail; the
	// handler surfaces 501 with a contextual message.
	reg := newRegistry(t, "docling-only")
	orch := newOrchestratorOrSkip(t)
	a := &Async{Registry: reg, Orchestrator: orch}

	req := httptest.NewRequest(http.MethodPost, "/process/async", strings.NewReader("hi"))
	req.Header.Set("Content-Type", "text/plain")
	rr := httptest.NewRecorder()
	a.SubmitProcess(rr, req)

	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("status: want 501 got %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "no engine for external facade") {
		t.Fatalf("expected facade-not-supported error, got %q", rr.Body.String())
	}
}

// referenced helpers that would otherwise be flagged unused in this
// file's narrow test surface.
var (
	_ = io.Copy
	_ = json.Marshal
)
