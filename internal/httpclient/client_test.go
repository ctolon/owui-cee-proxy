package httpclient

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ctolon/owui-cee-proxy/internal/config"
)

func TestNew_Defaults(t *testing.T) {
	t.Parallel()
	c, err := New(config.EngineConfig{
		URL: "http://example:8080",
	})
	require.NoError(t, err)
	require.NotNil(t, c.HTTP)
	require.Equal(t, 120*time.Second, c.RequestTimeout)
	require.Equal(t, "http://example:8080", c.BaseURL)
}

func TestNew_TLSPartialCertReturnsError(t *testing.T) {
	t.Parallel()
	_, err := New(config.EngineConfig{
		URL: "https://example:8080",
		HTTP: config.HTTPClientConfig{
			TLS: config.TLSClientConfig{CertFile: "/nope.pem"},
		},
	})
	require.Error(t, err)
}

func TestNew_ResponseHeaderTimeoutDefaultsToRequestTimeout(t *testing.T) {
	t.Parallel()
	// M7: when ResponseHeaderTimeout is unset (zero) we want it to
	// default to the configured RequestTimeout, not the legacy 30s,
	// so large-PDF backends that take >30s to emit headers don't
	// trip a transport-level timeout before the engine starts
	// streaming.
	c, err := New(config.EngineConfig{
		URL:            "http://example:8080",
		RequestTimeout: 4 * time.Minute,
	})
	require.NoError(t, err)
	tr, ok := c.HTTP.Transport.(*http.Transport)
	require.True(t, ok)
	require.Equal(t, 4*time.Minute, tr.ResponseHeaderTimeout)
}

func TestNew_ResponseHeaderTimeoutExplicitWins(t *testing.T) {
	t.Parallel()
	c, err := New(config.EngineConfig{
		URL:            "http://example:8080",
		RequestTimeout: 4 * time.Minute,
		HTTP: config.HTTPClientConfig{
			ResponseHeaderTimeout: 7 * time.Second,
		},
	})
	require.NoError(t, err)
	tr, ok := c.HTTP.Transport.(*http.Transport)
	require.True(t, ok)
	require.Equal(t, 7*time.Second, tr.ResponseHeaderTimeout)
}
