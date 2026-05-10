package tika

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"github.com/ctolon/owui-cee-proxy/internal/engine"
)

// Tika /tika/text returns either a single object or an array of
// objects. Each object includes "X-TIKA:content" plus arbitrary
// metadata keys. We extract content + key metadata into the
// Docling-compatible doclingDocument.
type tikaItem map[string]any

type doclingDocument struct {
	Filename   string         `json:"filename,omitempty"`
	MdContent  string         `json:"md_content"`
	TextContent string        `json:"text_content,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
}

type doclingEnvelope struct {
	Documents      []doclingDocument `json:"documents,omitempty"`
	Document       *doclingDocument  `json:"document,omitempty"`
	Status         string            `json:"status"`
	ProcessingTime float64           `json:"processing_time"`
	Errors         []string          `json:"errors"`
}

func parseTikaResponse(body []byte, filename string) (doclingDocument, error) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return doclingDocument{Filename: filename, MdContent: ""}, nil
	}

	switch body[0] {
	case '[':
		var items []tikaItem
		if err := json.Unmarshal(body, &items); err != nil {
			return doclingDocument{}, err
		}
		return mergeTikaItems(items, filename), nil
	case '{':
		var item tikaItem
		if err := json.Unmarshal(body, &item); err != nil {
			return doclingDocument{}, err
		}
		return mergeTikaItems([]tikaItem{item}, filename), nil
	default:
		// Plain text fallback.
		return doclingDocument{Filename: filename, MdContent: string(body), TextContent: string(body)}, nil
	}
}

func mergeTikaItems(items []tikaItem, filename string) doclingDocument {
	var content string
	metadata := map[string]any{}
	for i, it := range items {
		if v, ok := it["X-TIKA:content"].(string); ok {
			if content != "" {
				content += "\n\n"
			}
			content += v
		}
		// Only the first (root) item's metadata is preserved; embedded
		// document metadata is collapsed.
		if i == 0 {
			for k, v := range it {
				if k == "X-TIKA:content" {
					continue
				}
				metadata[k] = v
			}
		}
	}
	return doclingDocument{
		Filename:    filename,
		MdContent:   content,
		TextContent: content,
		Metadata:    metadata,
	}
}

func wrapDoclingResponse(docs []doclingDocument) (*engine.ConvertResponse, error) {
	env := doclingEnvelope{
		Status: "success",
		Errors: []string{},
	}
	switch {
	case len(docs) == 0:
		env.Status = "failure"
		env.Errors = []string{"no documents produced"}
	case len(docs) == 1:
		d := docs[0]
		env.Document = &d
	default:
		env.Documents = docs
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
