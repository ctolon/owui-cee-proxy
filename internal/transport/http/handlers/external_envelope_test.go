package handlers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ctolon/owui-cee-proxy/internal/engine"
)

// TestExternal_ErrorEnvelope_MissingContentType pins the C-9 fix:
// every External facade error path emits the OpenWebUI external-
// loader envelope (page_content + metadata.error), NOT the plain
// text http.Error shape that schema-strict clients reject.
func TestExternal_ErrorEnvelope_MissingContentType(t *testing.T) {
	t.Parallel()
	d := dummyEngineWithFacade{name: "default-eng", facade: engine.FacadeExternal}
	registry, err := engine.NewRegistry(map[engine.Name]engine.RegistryEntry{
		"default-eng": {Engine: d},
	}, "default-eng", engine.StrategyMimeThenExt)
	require.NoError(t, err)

	h := &External{Registry: registry}
	srv := httptest.NewServer(http.HandlerFunc(h.Process))
	defer srv.Close()

	// No Content-Type → external.go's first guard fires.
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPut, srv.URL, strings.NewReader(""))
	require.NoError(t, err)
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Equal(t, "application/json", strings.Split(resp.Header.Get("Content-Type"), ";")[0],
		"external facade errors MUST be JSON, never text/plain")

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	var env struct {
		PageContent string         `json:"page_content"`
		Metadata    map[string]any `json:"metadata"`
	}
	require.NoError(t, json.Unmarshal(body, &env), "external error envelope MUST decode as the OpenWebUI loader shape")
	require.Empty(t, env.PageContent)
	require.NotNil(t, env.Metadata)
	require.Contains(t, env.Metadata, "error")
}

// dummyEngineWithFacade satisfies engine.Engine for handler tests
// where only the facade matters.
type dummyEngineWithFacade struct {
	name   engine.Name
	facade engine.Facade
}

func (d dummyEngineWithFacade) Name() engine.Name { return d.name }
func (d dummyEngineWithFacade) URL() string       { return "http://" + string(d.name) + ".test" }
func (d dummyEngineWithFacade) Capabilities() engine.EngineCapabilities {
	return engine.EngineCapabilities{Facades: []engine.Facade{d.facade}}
}

func (d dummyEngineWithFacade) Convert(_ context.Context, _ *engine.ConvertRequest) (*engine.ConvertResponse, error) {
	return &engine.ConvertResponse{StatusCode: 200, Body: io.NopCloser(strings.NewReader(""))}, nil
}
func (d dummyEngineWithFacade) Health(_ context.Context) error { return nil }
