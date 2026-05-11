package handlers

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/ctolon/owui-cee-proxy/internal/engine"
	"github.com/ctolon/owui-cee-proxy/internal/engine/mimedetect"
	"github.com/ctolon/owui-cee-proxy/internal/engine/respond"
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
	// Resolver carries the merged (built-in + operator override)
	// extension map so the async multipart parse runs the same MIME
	// resolution contract as the sync convert handler. Composition
	// root passes the same instance to all three handlers.
	Resolver *mimedetect.Resolver
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
	// Baseline observability stamp — see Convert.handle for the rationale.
	// Async submit has its own collection of early-return paths
	// (orchestrator nil, payload parse failure, oversized blob, source-
	// without-HTTPSources support) that the access log must still
	// attribute to *some* engine.
	defaultEng := a.Registry.Default()
	r = r.WithContext(mw.WithEngine(r.Context(), string(defaultEng.Name()), defaultEng.URL()))
	r = r.WithContext(mw.WithPickDecision(r.Context(), string(engine.PickSourceDefault), string(a.Registry.Strategy())))

	if a.Orchestrator == nil {
		http.Error(w, "async tasks not enabled", http.StatusNotImplemented)
		return
	}
	payload, cleanup, err := tasks.PayloadFromRequest(r, source, a.maxBlobBytes(), a.Resolver)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, respond.NewDoclingError(err.Error()))
		return
	}
	defer cleanup()

	// Async endpoints belong to the docling facade.
	payload.Facade = string(engine.FacadeDocling)
	eng := a.Registry.Default()
	pickSrc := engine.PickSourceDefault
	if !source && len(payload.BlobKeys) > 0 {
		picked, src, perr := a.Registry.PickRouteVerbose(engine.FacadeDocling, engine.PickHint{
			MIME:     payload.BlobKeys[0].ContentType,
			Filename: payload.BlobKeys[0].Filename,
		})
		if perr == nil {
			eng = picked
			pickSrc = src
		}
	}

	// M14 (capability-based): reject source mode early when the chosen
	// engine does not advertise HTTPSources support, so callers see a
	// 400 instead of a synthetic 501 surfaced from the worker after a
	// Redis round-trip. This replaces the old hard-coded
	// "engine == Docling" check with the generic capability lookup.
	if source && len(payload.Sources) > 0 && !eng.Capabilities().HTTPSources {
		writeJSON(w, http.StatusBadRequest, respond.NewDoclingError(
			fmt.Sprintf("engine %s does not support http_sources", eng.Name()),
		))
		return
	}

	ctx := mw.WithEngine(r.Context(), string(eng.Name()), eng.URL())
	ctx = mw.WithPickDecision(ctx, string(pickSrc), string(a.Registry.Strategy()))
	if !source && len(payload.BlobKeys) > 0 {
		ctx = mw.WithFilename(ctx, payload.BlobKeys[0].Filename)
		ctx = mw.WithMimeType(ctx, payload.BlobKeys[0].ContentType)
		ctx = mw.WithFileCount(ctx, len(payload.BlobKeys))
	}
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
			writeJSON(w, http.StatusBadRequest, respond.NewDoclingError(err.Error()))
			return
		}
		writeJSON(
			w, http.StatusServiceUnavailable,
			respond.NewDoclingError(fmt.Sprintf("enqueue: %v", err)),
		)
		return
	}
	writeJSON(w, http.StatusAccepted, asyncAcceptedResponse{
		TaskID:     token,
		TaskStatus: "pending",
	})
}

// asyncAcceptedResponse is the 202 envelope returned when a task is
// enqueued. Hard-coding the shape here lets callers consume it via
// json.Unmarshal without guessing at the field names.
type asyncAcceptedResponse struct {
	TaskID     string `json:"task_id"`
	TaskStatus string `json:"task_status"`
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
	defer func() { _ = body.Close() }()
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
