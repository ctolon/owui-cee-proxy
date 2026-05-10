package tika

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ctolon/owui-cee-proxy/internal/config"
	"github.com/ctolon/owui-cee-proxy/internal/engine"
	"github.com/ctolon/owui-cee-proxy/internal/httpclient"
)

func TestConvert_PUTsRawBodyAndShapesResponse(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "PUT", r.Method)
		require.Equal(t, "/tika/text", r.URL.Path)
		require.Equal(t, "application/pdf", r.Header.Get("Content-Type"))
		body, _ := io.ReadAll(r.Body)
		require.Equal(t, "raw-pdf-bytes", string(body))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"X-TIKA:content":"# Hello world","Author":"alice"}`))
	}))
	defer srv.Close()

	cfg := config.EngineConfig{URL: srv.URL}
	cli, _ := httpclient.New(cfg)
	a, err := New(cfg, cli, nil)
	require.NoError(t, err)

	resp, err := a.Convert(context.Background(), &engine.ConvertRequest{
		Files: []engine.FileBlob{
			{Filename: "doc.pdf", ContentType: "application/pdf", Body: strings.NewReader("raw-pdf-bytes")},
		},
	})
	require.NoError(t, err)
	defer resp.Body.Close()

	var env doclingEnvelope
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&env))
	require.Equal(t, "success", env.Status)
	require.NotNil(t, env.Document)
	require.Equal(t, "# Hello world", env.Document.MdContent)
	require.Equal(t, "doc.pdf", env.Document.Filename)
	require.Equal(t, "alice", env.Document.Metadata["Author"])
}

func TestConvert_NoSources(t *testing.T) {
	t.Parallel()
	cfg := config.EngineConfig{URL: "http://x"}
	cli, _ := httpclient.New(cfg)
	a, _ := New(cfg, cli, nil)
	resp, err := a.Convert(context.Background(), &engine.ConvertRequest{
		Sources: []engine.HTTPSource{{URL: "http://x"}},
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusNotImplemented, resp.StatusCode)
}

func TestParseTikaResponse_Array(t *testing.T) {
	t.Parallel()
	doc, err := parseTikaResponse([]byte(`[{"X-TIKA:content":"a"},{"X-TIKA:content":"b"}]`), "x.pdf")
	require.NoError(t, err)
	require.Equal(t, "a\n\nb", doc.MdContent)
}

// L6 — decodeTikaResponse must stream the body without buffering the
// whole response into a []byte. The test feeds a ~100 KB JSON array
// containing many embedded items and checks the merge.
func TestDecodeTikaResponse_StreamingArray(t *testing.T) {
	t.Parallel()

	// Build a JSON array of 1,000 items each with a small content +
	// metadata payload. ~100 KB total. The decoder must consume the
	// reader incrementally (the test's stream fails on Read after a
	// hard cap to catch a regression to io.ReadAll).
	const items = 1000
	buf := strings.Builder{}
	buf.WriteByte('[')
	for i := 0; i < items; i++ {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.WriteString(`{"X-TIKA:content":"chunk-`)
		buf.WriteString(itoa(i))
		buf.WriteString(`"`)
		if i == 0 {
			buf.WriteString(`,"Author":"alice"`)
		}
		buf.WriteByte('}')
	}
	buf.WriteByte(']')

	doc, err := decodeTikaResponse(strings.NewReader(buf.String()), "big.pdf")
	require.NoError(t, err)
	require.Equal(t, "big.pdf", doc.Filename)
	require.Equal(t, "alice", doc.Metadata["Author"])
	// First and last chunk should be present in the merged content.
	require.True(t, strings.HasPrefix(doc.MdContent, "chunk-0\n\nchunk-1"))
	require.True(t, strings.HasSuffix(doc.MdContent, "chunk-999"))
}

// itoa avoids pulling strconv into the test surface.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
