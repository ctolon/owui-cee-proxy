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
	"github.com/ctolon/owui-cee-proxy/internal/engine/mimedetect"
	"github.com/ctolon/owui-cee-proxy/internal/spool"
)

// TypeConvert is the asynq task type for engine convert jobs.
const TypeConvert = "convert"

// Payload is the asynq task payload. It carries the engine name plus
// either a list of file blob references (Redis keys) or HTTPSources.
type Payload struct {
	Engine    string              `json:"engine"`
	Facade    string              `json:"facade,omitempty"`
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
// Enqueue is called. cleanup MUST be invoked by the caller. maxBlob
// caps any single file part (0 disables); validateBlobLimits in the
// orchestrator additionally enforces an aggregate cap.
//
// resolver MUST be non-nil for the multipart path. It applies the
// same MIME-resolution contract as the sync handlers (declared →
// sniff → filename extension) so async dispatch picks the same
// engine the sync path would have. JSON / source-mode bodies bypass
// the resolver entirely (no upload signal).
func PayloadFromRequest(r *http.Request, source bool, maxBlob int64, resolver *mimedetect.Resolver) (*Payload, func(), error) {
	if source {
		return payloadFromJSON(r)
	}
	return payloadFromMultipart(r, maxBlob, resolver)
}

// asyncSpoolThreshold mirrors the sync-path threshold (8 MiB) and is
// the in-memory cap before a multipart part is spilled to a temp file.
// Beyond the threshold the orchestrator will still need the bytes
// in-memory at Enqueue time (Redis SET takes []byte), so the savings
// here are bounded by the request's residency window — but we still
// avoid holding multiple 500 MiB slabs concurrently while parsing.
const asyncSpoolThreshold = 8 << 20

// asyncSniffHeadBytes is the prefix size used for the magic-byte
// sniff on each drained part buffer. Mirrors the sync handler's
// mimeSniffHeadBytes (512) — every mimetype.Detect signature fits.
const asyncSniffHeadBytes = 512

// asyncMaxFormFieldBytes caps a single non-file multipart form-field
// value on the async submit path. Same 1 MiB ceiling as the sync
// handler's maxFormFieldBytes — defends against the multipart DoS
// surface where an oversized form-field would otherwise reach
// io.ReadAll under the global BodyLimit (500 MiB by default).
const asyncMaxFormFieldBytes = 1 << 20

func payloadFromMultipart(r *http.Request, maxBlob int64, resolver *mimedetect.Resolver) (*Payload, func(), error) {
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
			// Spool to memory (small) or temp file (large). H1 + H2:
			// the per-blob cap is enforced BEFORE the bytes are written
			// past the threshold — fail fast.
			sr, err := spool.Part(part, asyncSpoolThreshold, maxBlob)
			fname := part.FileName()
			declaredCT := part.Header.Get("Content-Type")
			_ = part.Close()
			if err != nil {
				if errors.Is(err, spool.ErrTooLarge) {
					return nil, noop, fmt.Errorf("blob %q exceeds max_blob_bytes %d", fname, maxBlob)
				}
				return nil, noop, fmt.Errorf("copy part: %w", err)
			}
			// Drain the spool into a *bytes.Buffer; the orchestrator's
			// Redis SET requires a contiguous []byte. Spilling first to
			// disk still buys us peace from holding multiple 500-MiB
			// slabs in heap simultaneously while parsing concurrent
			// parts (and the disk file is unlinked on Close).
			buf := &bytes.Buffer{}
			if _, err := io.Copy(buf, sr); err != nil {
				_ = sr.Close()
				return nil, noop, fmt.Errorf("drain spool: %w", err)
			}
			_ = sr.Close()

			// MIME resolution mirrors the sync path: peek the head, run
			// the resolver, write the resolved MIME to the BlobRef so
			// the worker dispatches via the same engine the sync facade
			// would have picked. Closes the pre-existing gap where
			// async trusted the client's declared Content-Type verbatim.
			resolvedCT := declaredCT
			if resolver != nil {
				head := buf.Bytes()
				if len(head) > asyncSniffHeadBytes {
					head = head[:asyncSniffHeadBytes]
				}
				resolvedCT = resolver.Resolve(declaredCT, head, fname).MIME
			}

			bufs = append(bufs, buf)
			p.BlobKeys = append(p.BlobKeys, BlobRef{
				Filename:    fname,
				ContentType: resolvedCT,
				Size:        int64(buf.Len()),
			})
			continue
		}
		// Mirrors the sync convert handler's maxFormFieldBytes
		// cap. Async paths cap at the same 1 MiB ceiling so the
		// surface is symmetric — a hostile client cannot route via
		// /async/* to bypass the sync path's defence.
		body, err := io.ReadAll(io.LimitReader(part, int64(asyncMaxFormFieldBytes)+1))
		_ = part.Close()
		if err != nil {
			return nil, noop, err
		}
		if len(body) > asyncMaxFormFieldBytes {
			return nil, noop, fmt.Errorf("form field %q exceeds %d bytes", part.FormName(), asyncMaxFormFieldBytes)
		}
		p.Options[part.FormName()] = string(body)
	}
	// Stash buffers in payload via temporary field (filled in Enqueue).
	attachLocalBlobs(p, bufs)
	return p, noop, nil
}

// PayloadFromExternalRequest parses an async submission shaped like
// the synchronous external facade: raw body + Content-Type header
// (drives engine selection) + optional X-Filename. Produces a single-
// blob Payload whose Facade is set to FacadeExternal by the caller.
//
// resolver MUST be non-nil. The async path applies the same
// declared → sniff → filename-extension MIME contract as the sync
// External handler so dispatch lands on the same engine either way.
// maxBlob caps the body (0 disables; the global BodyLimit middleware
// is the request-wide ceiling).
func PayloadFromExternalRequest(r *http.Request, filename string, maxBlob int64, resolver *mimedetect.Resolver) (*Payload, func(), error) {
	declaredCT := r.Header.Get("Content-Type")
	if declaredCT == "" {
		return nil, noop, errors.New("missing Content-Type")
	}
	if filename == "" {
		filename = "upload"
	}

	// Drain the body through the spool so we get the same heap-
	// pressure budget as the sync path. After drain we still need
	// the bytes in-memory for Redis SET, so we re-buffer.
	sr, err := spool.Part(r.Body, asyncSpoolThreshold, maxBlob)
	if err != nil {
		if errors.Is(err, spool.ErrTooLarge) {
			return nil, noop, fmt.Errorf("blob %q exceeds max_blob_bytes %d", filename, maxBlob)
		}
		return nil, noop, fmt.Errorf("read body: %w", err)
	}
	buf := &bytes.Buffer{}
	if _, err := io.Copy(buf, sr); err != nil {
		_ = sr.Close()
		return nil, noop, fmt.Errorf("drain spool: %w", err)
	}
	_ = sr.Close()

	resolvedCT := declaredCT
	if resolver != nil {
		head := buf.Bytes()
		if len(head) > asyncSniffHeadBytes {
			head = head[:asyncSniffHeadBytes]
		}
		resolvedCT = resolver.Resolve(declaredCT, head, filename).MIME
	}

	p := &Payload{
		BlobKeys: []BlobRef{{
			Filename:    filename,
			ContentType: resolvedCT,
			Size:        int64(buf.Len()),
		}},
		Options: map[string]string{},
	}
	attachLocalBlobs(p, []*bytes.Buffer{buf})
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
