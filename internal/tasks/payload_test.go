package tasks

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ctolon/owui-cee-proxy/internal/engine/mimedetect"
)

// cfbHead — see mimedetect/detect_test.go::cfbHead. Replicated here
// to avoid cross-package test fixture imports.
var cfbHead = []byte{
	0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1,
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
}

// pdfHead — same idea: minimal PDF header for the magic-byte sniff.
var pdfHead = []byte("%PDF-1.7\n%\xe2\xe3\xcf\xd3\n")

// buildPartRequest constructs an *http.Request carrying a single
// multipart file part with the given declared MIME, filename, and
// body. Used to exercise payloadFromMultipart directly.
func buildPartRequest(t *testing.T, declared, filename string, body []byte) *http.Request {
	t.Helper()
	buf := &bytes.Buffer{}
	mw := multipart.NewWriter(buf)
	h := map[string][]string{
		"Content-Disposition": {`form-data; name="files"; filename="` + filename + `"`},
		"Content-Type":        {declared},
	}
	w, err := mw.CreatePart(h)
	require.NoError(t, err)
	_, err = w.Write(body)
	require.NoError(t, err)
	require.NoError(t, mw.Close())

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/", buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

// TestPayloadFromMultipart_ResolvesMIMEPerPart — async path MUST run
// the same MIME-resolution contract as the sync handlers. With a
// generic declared type + a .msg body + filename, the resolved MIME
// must disambiguate to application/vnd.ms-outlook so the worker
// dispatches to the same engine the sync facade would have picked.
func TestPayloadFromMultipart_ResolvesMIMEPerPart(t *testing.T) {
	t.Parallel()
	r := buildPartRequest(t, "application/octet-stream", "report.msg", cfbHead)
	resolver := mimedetect.NewResolver(nil)

	p, _, err := payloadFromMultipart(r, 0, resolver)
	require.NoError(t, err)
	require.Len(t, p.BlobKeys, 1)
	require.Equal(t, "application/vnd.ms-outlook", p.BlobKeys[0].ContentType,
		"async parse must run the Resolver and write the disambiguated MIME")
	require.Equal(t, "report.msg", p.BlobKeys[0].Filename)
}

// TestPayloadFromMultipart_TrustsConcreteDeclared — a specific
// declared Content-Type MUST flow through verbatim; the resolver is
// not consulted for the trust-the-client fast path.
func TestPayloadFromMultipart_TrustsConcreteDeclared(t *testing.T) {
	t.Parallel()
	r := buildPartRequest(t, "application/pdf", "x.pdf", pdfHead)
	resolver := mimedetect.NewResolver(nil)

	p, _, err := payloadFromMultipart(r, 0, resolver)
	require.NoError(t, err)
	require.Len(t, p.BlobKeys, 1)
	require.Equal(t, "application/pdf", p.BlobKeys[0].ContentType)
}

// TestPayloadFromMultipart_NilResolverTrustsDeclared — when the
// composition root forgets to inject a resolver (or in transitional
// tests), the parse must NOT panic and must keep the client's
// declared MIME. Safety net for callers that haven't migrated.
func TestPayloadFromMultipart_NilResolverTrustsDeclared(t *testing.T) {
	t.Parallel()
	r := buildPartRequest(t, "application/pdf", "x.pdf", pdfHead)

	p, _, err := payloadFromMultipart(r, 0, nil /* resolver */)
	require.NoError(t, err)
	require.Equal(t, "application/pdf", p.BlobKeys[0].ContentType)
}
