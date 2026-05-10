package httpclient

import (
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
