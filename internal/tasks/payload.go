// Package tasks implements the Redis-backed (asynq) task system that
// powers the async endpoints. Bodies that travel between the API
// replica and the worker replica are persisted to Redis blobs (under
// TTL) so the proxy itself stays stateless and any replica can serve
// status / result for any task.
package tasks

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"

	"github.com/ctolon/owui-cee-proxy/internal/engine"
)

// TypeConvert is the asynq task type for engine convert jobs.
const TypeConvert = "convert"

// Payload is the asynq task payload. It carries the engine name plus
// either a list of file blob references (Redis keys) or HTTPSources.
type Payload struct {
	Engine    string              `json:"engine"`
	RequestID string              `json:"request_id"`
	BlobKeys  []BlobRef           `json:"blob_keys,omitempty"`
	Sources   []engine.HTTPSource `json:"sources,omitempty"`
	Options   map[string]string   `json:"options,omitempty"`

	// localBlobs is filled by PayloadFromRequest with the buffered
	// multipart bodies. The orchestrator drains it via takeBuffers
	// during Enqueue. Unexported → not serialised to Redis.
	localBlobs *localBlobs `json:"-"`
}

type BlobRef struct {
	Key         string `json:"key"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
}

// PayloadFromRequest parses the inbound HTTP request into a Payload
// without persisting blob bodies; the orchestrator persists them when
// Enqueue is called. cleanup MUST be invoked by the caller.
func PayloadFromRequest(r *http.Request, source bool) (*Payload, func(), error) {
	if source {
		return payloadFromJSON(r)
	}
	return payloadFromMultipart(r)
}

func payloadFromMultipart(r *http.Request) (*Payload, func(), error) {
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return nil, noop, fmt.Errorf("parse content-type: %w", err)
	}
	if !strings.HasPrefix(mediaType, "multipart/") {
		return nil, noop, errors.New("content-type must be multipart/form-data")
	}
	mr := multipart.NewReader(r.Body, params["boundary"])

	p := &Payload{Options: map[string]string{}}
	bufs := make([]*bytes.Buffer, 0, 4)
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, noop, fmt.Errorf("read part: %w", err)
		}
		if part.FileName() != "" {
			buf := &bytes.Buffer{}
			if _, err := io.Copy(buf, part); err != nil {
				_ = part.Close()
				return nil, noop, fmt.Errorf("copy part: %w", err)
			}
			_ = part.Close()
			bufs = append(bufs, buf)
			p.BlobKeys = append(p.BlobKeys, BlobRef{
				Filename:    part.FileName(),
				ContentType: part.Header.Get("Content-Type"),
				Size:        int64(buf.Len()),
			})
			continue
		}
		body, err := io.ReadAll(part)
		_ = part.Close()
		if err != nil {
			return nil, noop, err
		}
		p.Options[part.FormName()] = string(body)
	}
	// Stash buffers in payload via temporary field (filled in Enqueue).
	attachLocalBlobs(p, bufs)
	return p, noop, nil
}

type sourcePayload struct {
	HTTPSources []engine.HTTPSource `json:"http_sources"`
	Options     map[string]string   `json:"options"`
}

func payloadFromJSON(r *http.Request) (*Payload, func(), error) {
	var body sourcePayload
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return nil, noop, err
	}
	if len(body.HTTPSources) == 0 {
		return nil, noop, errors.New("http_sources required")
	}
	return &Payload{Sources: body.HTTPSources, Options: body.Options}, noop, nil
}

func noop() {}

// localBlobs is an in-process side-channel used by the orchestrator to
// upload buffered blob bodies after parsing. We deliberately keep this
// off Payload (which is JSON-serialised onto Redis) so blob bytes do
// not double-encode.
type localBlobsKey struct{}

type localBlobs struct {
	Buffers []*bytes.Buffer
}

func attachLocalBlobs(p *Payload, bufs []*bytes.Buffer) {
	p.localBlobs = &localBlobs{Buffers: bufs}
}

// Payload exposes a private slot only consumable inside this package.
// The exported Payload struct does not contain it because it must be
// JSON-serialisable to Redis without double-buffering.
func (p *Payload) takeBuffers() []*bytes.Buffer {
	if p.localBlobs == nil {
		return nil
	}
	bufs := p.localBlobs.Buffers
	p.localBlobs = nil
	return bufs
}
