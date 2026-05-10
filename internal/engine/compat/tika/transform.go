package tika

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/ctolon/owui-cee-proxy/internal/engine"
)

// Tika /tika/text returns either a single object or an array of
// objects. Each object includes "X-TIKA:content" plus arbitrary
// metadata keys. We extract content + key metadata into the
// adapter's intermediate "item" shape, then translate to whichever
// facade envelope the caller expects.
type tikaItem map[string]any

// item is the adapter's intermediate representation; we collapse it to
// the right facade-specific shape in wrapDoclingResponse / wrapExternalResponse.
type item struct {
	Filename    string         `json:"filename,omitempty"`
	MdContent   string         `json:"md_content"`
	TextContent string         `json:"text_content,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}

type doclingEnvelope struct {
	Documents      []item   `json:"documents,omitempty"`
	Document       *item    `json:"document,omitempty"`
	Status         string   `json:"status"`
	ProcessingTime float64  `json:"processing_time"`
	Errors         []string `json:"errors"`
}

// externalDoc mirrors OpenWebUI's external loader response shape.
type externalDoc struct {
	PageContent string         `json:"page_content"`
	Metadata    map[string]any `json:"metadata"`
}

// decodeTikaResponse streams the upstream Tika body. Tika /tika/text
// emits either a JSON array, a single JSON object, or plain text. We
// peek the first non-whitespace byte to dispatch.
func decodeTikaResponse(r io.Reader, filename string) (item, error) {
	br := bufio.NewReader(r)
	for {
		b, err := br.ReadByte()
		if errors.Is(err, io.EOF) {
			return item{Filename: filename, MdContent: ""}, nil
		}
		if err != nil {
			return item{}, err
		}
		if b == ' ' || b == '\t' || b == '\r' || b == '\n' {
			continue
		}
		if err := br.UnreadByte(); err != nil {
			return item{}, err
		}
		switch b {
		case '[':
			var items []tikaItem
			if err := json.NewDecoder(br).Decode(&items); err != nil {
				return item{}, err
			}
			return mergeTikaItems(items, filename), nil
		case '{':
			var single tikaItem
			if err := json.NewDecoder(br).Decode(&single); err != nil {
				return item{}, err
			}
			return mergeTikaItems([]tikaItem{single}, filename), nil
		default:
			rest, err := io.ReadAll(br)
			if err != nil {
				return item{}, err
			}
			s := string(rest)
			return item{Filename: filename, MdContent: s, TextContent: s}, nil
		}
	}
}

func mergeTikaItems(items []tikaItem, filename string) item {
	var content string
	metadata := map[string]any{}
	for i, it := range items {
		if v, ok := it["X-TIKA:content"].(string); ok {
			if content != "" {
				content += "\n\n"
			}
			content += v
		}
		if i == 0 {
			for k, v := range it {
				if k == "X-TIKA:content" {
					continue
				}
				metadata[k] = v
			}
		}
	}
	return item{
		Filename:    filename,
		MdContent:   content,
		TextContent: content,
		Metadata:    metadata,
	}
}

func wrapDoclingResponse(items []item) (*engine.ConvertResponse, error) {
	env := doclingEnvelope{Status: "success", Errors: []string{}}
	switch {
	case len(items) == 0:
		env.Status = "failure"
		env.Errors = []string{"no documents produced"}
	case len(items) == 1:
		d := items[0]
		env.Document = &d
	default:
		env.Documents = items
	}
	body, err := json.Marshal(env)
	if err != nil {
		return nil, err
	}
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	return &engine.ConvertResponse{
		StatusCode: http.StatusOK,
		Headers:    h,
		Body:       io.NopCloser(bytes.NewReader(body)),
	}, nil
}

func wrapExternalResponse(items []item) (*engine.ConvertResponse, error) {
	docs := make([]externalDoc, 0, len(items))
	for _, it := range items {
		md := it.Metadata
		if md == nil {
			md = map[string]any{}
		}
		if it.Filename != "" {
			md["filename"] = it.Filename
		}
		docs = append(docs, externalDoc{PageContent: it.MdContent, Metadata: md})
	}
	var body []byte
	var err error
	switch len(docs) {
	case 1:
		body, err = json.Marshal(docs[0])
	default:
		body, err = json.Marshal(docs)
	}
	if err != nil {
		return nil, err
	}
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	return &engine.ConvertResponse{
		StatusCode: http.StatusOK,
		Headers:    h,
		Body:       io.NopCloser(bytes.NewReader(body)),
	}, nil
}
