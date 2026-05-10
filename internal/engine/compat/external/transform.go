package external

import (
	"bytes"
	"encoding/json"
)

// externalDoc mirrors OpenWebUI's external loader response item.
type externalDoc struct {
	PageContent string         `json:"page_content"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}

// decodeExternalResponse accepts either a single object or a list of
// objects in OpenWebUI's external-loader shape and returns a flat
// slice. Unknown shapes are coerced into a single doc with the raw
// response in metadata.error.
func decodeExternalResponse(raw []byte) ([]externalDoc, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return []externalDoc{{PageContent: ""}}, nil
	}
	switch raw[0] {
	case '[':
		var items []externalDoc
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, err
		}
		return items, nil
	case '{':
		var one externalDoc
		if err := json.Unmarshal(raw, &one); err != nil {
			return nil, err
		}
		return []externalDoc{one}, nil
	default:
		// Plain text fallback: treat the body as page_content.
		return []externalDoc{{PageContent: string(raw)}}, nil
	}
}
