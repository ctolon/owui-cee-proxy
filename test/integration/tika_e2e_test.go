//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/ctolon/owui-cee-proxy/internal/config"
)

func TestTika_FacadeReturnsDoclingShape(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	req := testcontainers.ContainerRequest{
		Image:        "apache/tika:latest-full",
		ExposedPorts: []string{"9998/tcp"},
		WaitingFor:   wait.ForHTTP("/version").WithPort("9998/tcp").WithStartupTimeout(2 * time.Minute),
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	require.NoError(t, err)
	defer func() { _ = c.Terminate(ctx) }()
	host, err := c.Host(ctx)
	require.NoError(t, err)
	port, err := c.MappedPort(ctx, "9998")
	require.NoError(t, err)
	tikaURL := "http://" + host + ":" + port.Port()

	cfg := config.Default()
	cfg.Routing.DefaultCEEEngine = "tika"
	cfg.Engines.Tika.Enable = true
	cfg.Engines.Tika.URL = tikaURL

	proxy := newProxyServer(t, cfg)
	waitForReadiness(t, proxy.URL+"/healthz", 30*time.Second)

	body, contentType := buildMultipart(t, fixturePath(t, "sample.txt"), "files")
	resp, err := newClient().Post(proxy.URL+"/v1/convert/file", contentType, body)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var env struct {
		Status   string `json:"status"`
		Document struct {
			MdContent string `json:"md_content"`
			Filename  string `json:"filename"`
		} `json:"document"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&env))
	require.Equal(t, "success", env.Status)
	require.NotEmpty(t, env.Document.MdContent)
	require.Equal(t, "sample.txt", env.Document.Filename)
}

func TestTika_PassthroughReturnsNativeJSON(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	req := testcontainers.ContainerRequest{
		Image:        "apache/tika:latest-full",
		ExposedPorts: []string{"9998/tcp"},
		WaitingFor:   wait.ForHTTP("/version").WithPort("9998/tcp").WithStartupTimeout(2 * time.Minute),
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	require.NoError(t, err)
	defer func() { _ = c.Terminate(ctx) }()
	host, _ := c.Host(ctx)
	port, _ := c.MappedPort(ctx, "9998")
	tikaURL := "http://" + host + ":" + port.Port()

	cfg := config.Default()
	cfg.Routing.DefaultCEEEngine = "tika"
	cfg.Engines.Tika.Enable = true
	cfg.Engines.Tika.URL = tikaURL

	proxy := newProxyServer(t, cfg)
	waitForReadiness(t, proxy.URL+"/healthz", 30*time.Second)

	// Send native PUT to /tika/text via passthrough.
	contents, err := os.ReadFile(fixturePath(t, "sample.txt"))
	require.NoError(t, err)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPut, proxy.URL+"/tika/tika/text", bytes.NewReader(contents))
	require.NoError(t, err)
	httpReq.Header.Set("Content-Type", "text/plain")
	httpReq.Header.Set("Accept", "text/plain")
	resp, err := newClient().Do(httpReq)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	require.Contains(t, string(body), "hello") // sample.txt contains "hello"
}

func buildMultipart(t *testing.T, path, field string) (io.Reader, string) {
	t.Helper()
	buf := &bytes.Buffer{}
	mw := multipart.NewWriter(buf)
	f, err := os.Open(path) //nolint:gosec
	require.NoError(t, err)
	defer f.Close()
	fw, err := mw.CreateFormFile(field, filepath.Base(path))
	require.NoError(t, err)
	_, err = io.Copy(fw, f)
	require.NoError(t, err)
	require.NoError(t, mw.Close())
	return buf, mw.FormDataContentType()
}

func fixturePath(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join("fixtures", name)
}
