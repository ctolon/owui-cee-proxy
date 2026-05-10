package handlers

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/ctolon/owui-cee-proxy/internal/engine"
	"github.com/ctolon/owui-cee-proxy/internal/tasks"
	mw "github.com/ctolon/owui-cee-proxy/internal/transport/http/middleware"
)

// Async exposes the Docling-shaped async endpoints backed by Redis +
// asynq so any replica can poll/fetch any task.
type Async struct {
	Registry     engine.Registry
	Orchestrator *tasks.Orchestrator
	// APIKeyHeader is the configured proxy API-key header name. Used to
	// bind a task token to the caller's key fingerprint.
	APIKeyHeader string
}

// maxBlobBytes returns the per-blob cap from the orchestrator config
// (0 disables).
func (a *Async) maxBlobBytes() int64 {
	if a.Orchestrator == nil {
		return 0
	}
	return a.Orchestrator.Config().MaxBlobBytes
}

func (a *Async) SubmitFile(w http.ResponseWriter, r *http.Request) {
	a.submit(w, r, false)
}

func (a *Async) SubmitSource(w http.ResponseWriter, r *http.Request) {
	a.submit(w, r, true)
}

func (a *Async) submit(w http.ResponseWriter, r *http.Request, source bool) {
	if a.Orchestrator == nil {
		http.Error(w, "async tasks not enabled", http.StatusNotImplemented)
		return
	}
	payload, cleanup, err := tasks.PayloadFromRequest(r, source, a.maxBlobBytes())
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"status": "failure",
			"errors": []map[string]string{{"message": err.Error()}},
		})
		return
	}
	defer cleanup()

	eng := a.Registry.Default()
	if !source && len(payload.BlobKeys) > 0 {
		eng = a.Registry.Pick(payload.BlobKeys[0].ContentType)
	}

	// M14: source-mode is only supported by Docling. Reject early so
	// callers see a 400 instead of a synthetic 501 surfaced from the
	// worker after a Redis round-trip.
	if source && len(payload.Sources) > 0 && eng.Name() != engine.Docling {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"status": "failure",
			"errors": []map[string]string{{"message": fmt.Sprintf("engine %s does not support http_sources; use docling", eng.Name())}},
		})
		return
	}

	ctx := mw.WithEngine(r.Context(), string(eng.Name()), "")
	payload.Engine = string(eng.Name())
	payload.RequestID = mw.IDFrom(ctx)

	apiKey := ""
	if a.APIKeyHeader != "" {
		apiKey = r.Header.Get(a.APIKeyHeader)
	}

	token, err := a.Orchestrator.Enqueue(ctx, payload, apiKey)
	if err != nil {
		// Blob limit / per-blob validation errors map to 400.
		if isClientError(err) {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"status": "failure",
				"errors": []map[string]string{{"message": err.Error()}},
			})
			return
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status": "failure",
			"errors": []map[string]string{{"message": fmt.Sprintf("enqueue: %v", err)}},
		})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"task_id":     token,
		"task_status": "pending",
	})
}

func (a *Async) Poll(w http.ResponseWriter, r *http.Request) {
	if a.Orchestrator == nil {
		http.Error(w, "async tasks not enabled", http.StatusNotImplemented)
		return
	}
	token := chi.URLParam(r, "id")
	apiKey := ""
	if a.APIKeyHeader != "" {
		apiKey = r.Header.Get(a.APIKeyHeader)
	}
	info, err := a.Orchestrator.Status(r.Context(), token, apiKey)
	if err != nil {
		if errors.Is(err, tasks.ErrNotFound) {
			http.Error(w, "task not found", http.StatusNotFound)
			return
		}
		http.Error(w, "lookup failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// allowedResultContentTypes captures the set of content types we'll
// render inline in the response. Anything else is downgraded to
// application/octet-stream and forced to download (H4).
var allowedResultContentTypes = map[string]struct{}{
	"application/json": {},
	"text/plain":       {},
	"text/markdown":    {},
	"text/html":        {},
}

func (a *Async) Result(w http.ResponseWriter, r *http.Request) {
	if a.Orchestrator == nil {
		http.Error(w, "async tasks not enabled", http.StatusNotImplemented)
		return
	}
	token := chi.URLParam(r, "id")
	apiKey := ""
	if a.APIKeyHeader != "" {
		apiKey = r.Header.Get(a.APIKeyHeader)
	}
	body, contentType, err := a.Orchestrator.Result(r.Context(), token, apiKey)
	if err != nil {
		if errors.Is(err, tasks.ErrNotFound) {
			http.Error(w, "result not ready", http.StatusNotFound)
			return
		}
		http.Error(w, "lookup failed", http.StatusInternalServerError)
		return
	}
	defer body.Close()
	writeResultResponse(w, body, contentType)
}

// writeResultResponse renders the body to w applying the H4 hardening
// rules: always set X-Content-Type-Options:nosniff; only render an
// allowlisted Content-Type inline; otherwise force a download via
// application/octet-stream + Content-Disposition: attachment.
func writeResultResponse(w http.ResponseWriter, body io.Reader, contentType string) {
	w.Header().Set("X-Content-Type-Options", "nosniff")

	out := strings.ToLower(strings.TrimSpace(splitMediaType(contentType)))
	if out == "" {
		out = "application/json"
	}
	if _, ok := allowedResultContentTypes[out]; !ok {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", "attachment; filename=\"result.bin\"")
	} else {
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, body)
}

// splitMediaType returns the media type portion of a Content-Type
// header (eg. "text/html; charset=utf-8" → "text/html"). Avoids
// pulling in mime.ParseMediaType for an extra error path.
func splitMediaType(ct string) string {
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		return ct[:i]
	}
	return ct
}

// isClientError returns true for orchestrator errors that should map
// to a 4xx, principally blob-cap violations.
func isClientError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "exceeds max_blob_bytes") ||
		strings.Contains(msg, "exceeds max_total_bytes")
}

