package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/hibiken/asynq"
	"github.com/rs/zerolog"

	"github.com/ctolon/owui-cee-proxy/internal/config"
	"github.com/ctolon/owui-cee-proxy/internal/engine"
)

// Worker runs the asynq server consuming TypeConvert tasks. Multiple
// replicas can run workers concurrently; asynq's Redis-backed lease
// guarantees a task is processed by exactly one consumer at a time.
type Worker struct {
	srv      *asynq.Server
	orch     *Orchestrator
	registry engine.Registry
	logger   zerolog.Logger
}

func NewWorker(cfg config.TasksConfig, orch *Orchestrator, registry engine.Registry, logger zerolog.Logger) (*Worker, error) {
	opt, err := asynq.ParseRedisURI(string(cfg.RedisURL))
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}
	weights := cfg.QueueWeights
	if len(weights) == 0 {
		weights = map[string]int{"default": 6, "low": 3, "critical": 1}
	}
	srv := asynq.NewServer(opt, asynq.Config{
		Concurrency: cfg.QueueConcurrency,
		Queues:      weights,
		ErrorHandler: asynq.ErrorHandlerFunc(func(ctx context.Context, t *asynq.Task, err error) {
			logger.Error().Err(err).Str("task_type", t.Type()).Msg("task_failed")
		}),
		Logger: zerologAsynqLogger{l: logger},
	})
	return &Worker{srv: srv, orch: orch, registry: registry, logger: logger}, nil
}

func (w *Worker) Start() error {
	mux := asynq.NewServeMux()
	mux.HandleFunc(TypeConvert, w.handleConvert)
	return w.srv.Start(mux)
}

func (w *Worker) Shutdown() {
	w.srv.Shutdown()
}

func (w *Worker) handleConvert(ctx context.Context, t *asynq.Task) error {
	taskID, _ := asynq.GetTaskID(ctx)
	var p Payload
	if err := json.Unmarshal(t.Payload(), &p); err != nil {
		w.recordFailure(taskID, "", err, "unmarshal")
		return fmt.Errorf("unmarshal: %w: %w", err, asynq.SkipRetry)
	}
	eng, err := w.registry.Get(engine.Name(p.Engine))
	if err != nil {
		w.recordFailure(taskID, p.RequestID, err, "unknown_engine")
		return fmt.Errorf("engine %s: %w: %w", p.Engine, err, asynq.SkipRetry)
	}

	// Lifecycle: task left the queue, observability comes online.
	w.orch.lifecycle.RecordTaskLifecycle("dequeued", "success")
	w.logger.Info().
		Str("event", "task_dequeued").
		Str("task_id", taskID).
		Str("engine", p.Engine).
		Str("request_id", p.RequestID).
		Int("file_count", len(p.BlobKeys)).
		Msg("async task dequeued")

	files := make([]engine.FileBlob, 0, len(p.BlobKeys))
	for _, ref := range p.BlobKeys {
		b, err := w.orch.LoadBlob(ctx, ref.Key)
		if err != nil {
			w.recordFailure(taskID, p.RequestID, err, "blob_load")
			return fmt.Errorf("load blob %s: %w", ref.Key, err)
		}
		files = append(files, engine.FileBlob{
			Filename:    ref.Filename,
			ContentType: ref.ContentType,
			Size:        ref.Size,
			Body:        bytes.NewReader(b),
		})
	}

	facade := engine.Facade(p.Facade)
	if facade == "" {
		facade = engine.FacadeDocling
	}
	req := &engine.ConvertRequest{
		Files:     files,
		Sources:   p.Sources,
		Options:   p.Options,
		RequestID: p.RequestID,
		Facade:    facade,
	}

	resp, err := eng.Convert(ctx, req)
	if err != nil {
		w.recordFailure(taskID, p.RequestID, err, "engine_convert")
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		w.recordFailure(taskID, p.RequestID, err, "read_response")
		return err
	}
	if err := w.orch.SaveResult(ctx, taskID, resp.Headers.Get("Content-Type"), body); err != nil {
		w.recordFailure(taskID, p.RequestID, err, "save_result")
		return err
	}
	w.orch.lifecycle.RecordTaskLifecycle("completed", "success")
	w.logger.Info().
		Str("event", "task_completed").
		Str("task_id", taskID).
		Str("engine", p.Engine).
		Str("request_id", p.RequestID).
		Int("status", resp.StatusCode).
		Msg("async task completed")
	return nil
}

// recordFailure centralises the failed-task observability surface.
// reason maps to a stable label value so dashboards alert on
// distinct failure categories (blob_load failures point at Redis;
// engine_convert at the backend).
func (w *Worker) recordFailure(taskID, requestID string, err error, reason string) {
	w.orch.lifecycle.RecordTaskLifecycle("failed", reason)
	w.logger.Error().
		Err(err).
		Str("event", "task_failed").
		Str("task_id", taskID).
		Str("request_id", requestID).
		Str("reason", reason).
		Msg("async task failed")
}

type zerologAsynqLogger struct{ l zerolog.Logger }

func (z zerologAsynqLogger) Debug(args ...any) { z.l.Debug().Msg(sprint(args)) }
func (z zerologAsynqLogger) Info(args ...any)  { z.l.Info().Msg(sprint(args)) }
func (z zerologAsynqLogger) Warn(args ...any)  { z.l.Warn().Msg(sprint(args)) }
func (z zerologAsynqLogger) Error(args ...any) { z.l.Error().Msg(sprint(args)) }
func (z zerologAsynqLogger) Fatal(args ...any) { z.l.Fatal().Msg(sprint(args)) }

func sprint(a []any) string {
	if len(a) == 0 {
		return ""
	}
	return fmt.Sprint(a...)
}
