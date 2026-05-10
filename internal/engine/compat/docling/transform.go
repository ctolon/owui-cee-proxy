package docling

import (
	"encoding/json"

	"github.com/ctolon/owui-cee-proxy/internal/engine"
)

// sourcePayload mirrors Docling's /v1/convert/source request schema.
type sourcePayload struct {
	HTTPSources []sourceItem      `json:"http_sources,omitempty"`
	Options     map[string]string `json:"options,omitempty"`
}

type sourceItem struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
}

func buildSourceJSON(req *engine.ConvertRequest) ([]byte, error) {
	items := make([]sourceItem, 0, len(req.Sources))
	for _, s := range req.Sources {
		items = append(items, sourceItem{URL: s.URL, Headers: s.Headers})
	}
	payload := sourcePayload{HTTPSources: items, Options: req.Options}
	return json.Marshal(payload)
}
