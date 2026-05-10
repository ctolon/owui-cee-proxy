package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/ctolon/owui-cee-proxy/internal/engine"
	"github.com/ctolon/owui-cee-proxy/internal/tasks"
	mw "github.com/ctolon/owui-cee-proxy/internal/transport/http/middleware"
)

// Async exposes the Docling-shaped async endpoints backed by Redis +
// asynq so any replica can poll/fetch any task.
type Async struct {
	Registry      engine.Registry
	Orchestrator  *tasks.Orchestrator
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
	payload, cleanup, err := tasks.PayloadFromRequest(r, source)
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
	ctx := mw.WithEngine(r.Context(), string(eng.Name()), "")
	payload.Engine = string(eng.Name())
	payload.RequestID = mw.IDFrom(ctx)

	id, err := a.Orchestrator.Enqueue(ctx, payload)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status": "failure",
			"errors": []map[string]string{{"message": fmt.Sprintf("enqueue: %v", err)}},
		})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"task_id":     id,
		"task_status": "pending",
	})
}

func (a *Async) Poll(w http.ResponseWriter, r *http.Request) {
	if a.Orchestrator == nil {
		http.Error(w, "async tasks not enabled", http.StatusNotImplemented)
		return
	}
	id := chi.URLParam(r, "id")
	info, err := a.Orchestrator.Status(r.Context(), id)
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

func (a *Async) Result(w http.ResponseWriter, r *http.Request) {
	if a.Orchestrator == nil {
		http.Error(w, "async tasks not enabled", http.StatusNotImplemented)
		return
	}
	id := chi.URLParam(r, "id")
	body, contentType, err := a.Orchestrator.Result(r.Context(), id)
	if err != nil {
		if errors.Is(err, tasks.ErrNotFound) {
			http.Error(w, "result not ready", http.StatusNotFound)
			return
		}
		http.Error(w, "lookup failed", http.StatusInternalServerError)
		return
	}
	defer body.Close()
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, body)
}

// dummy reference to keep encoding/json import used for build robustness.
var _ = json.Marshal
