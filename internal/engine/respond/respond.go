// Package respond owns the typed JSON shapes the proxy returns to
// clients (facade error envelopes) and the typed shapes adapters
// produce when they need to synthesise a response (e.g., engine
// returned an unparseable body or the breaker is open).
//
// Before this package existed, six call sites built error responses
// via `map[string]any` literals. The shapes drifted across handlers,
// which made schema-aware downstreams (OpenWebUI's strict Pydantic
// loader) fail in interesting ways. Each adapter and handler must
// route through one of the typed builders below.
package respond

// DoclingError is the canonical error envelope returned by the
// Docling facade. Matches the upstream Docling Serve schema so
// OpenWebUI's Pydantic decoder accepts the response.
type DoclingError struct {
	Status string        `json:"status"`
	Errors []ErrorDetail `json:"errors"`
}

// ErrorDetail is a single error entry inside DoclingError.Errors.
// Kept open for future per-error metadata (severity, code, span)
// without breaking the wire format.
type ErrorDetail struct {
	Message string `json:"message"`
}

// ExternalError is the OpenWebUI external-loader error envelope. The
// loader expects a single object with page_content + metadata, even
// on failure — there's no separate error shape in that contract.
type ExternalError struct {
	PageContent string         `json:"page_content"`
	Metadata    map[string]any `json:"metadata"`
}

// NewDoclingError builds a DoclingError carrying a single message.
// Callers that need multiple errors construct DoclingError directly.
func NewDoclingError(msg string) DoclingError {
	return DoclingError{
		Status: "failure",
		Errors: []ErrorDetail{{Message: msg}},
	}
}

// NewExternalError builds an ExternalError whose metadata.error is
// set to msg. page_content is intentionally empty — the loader
// contract dictates that callers detect failure via the metadata
// shape, not by sniffing page_content for an error string.
func NewExternalError(msg string) ExternalError {
	return ExternalError{
		PageContent: "",
		Metadata:    map[string]any{"error": msg},
	}
}
