package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"

	"github.com/ctolon/owui-cee-proxy/internal/config"
	"github.com/ctolon/owui-cee-proxy/internal/engine"
)

var ErrNotFound = errors.New("tasks: not found")

// Orchestrator is the API-side handle on the asynq client and Redis
// blob store. The worker side runs in the same binary but on a
// different lifecycle (NewWorker).
type Orchestrator struct {
	cfg      config.TasksConfig
	client   *asynq.Client
	inspector *asynq.Inspector
	redis    *redis.Client
	registry engine.Registry
}

func NewOrchestrator(cfg config.TasksConfig, registry engine.Registry) (*Orchestrator, error) {
	if !cfg.Enabled {
		return nil, errors.New("tasks: not enabled")
	}
	if cfg.RedisURL == "" {
		return nil, errors.New("tasks: REDIS_URL is required")
	}
	opt, err := asynq.ParseRedisURI(cfg.RedisURL)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}
	rOpt, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}
	return &Orchestrator{
		cfg:       cfg,
		client:    asynq.NewClient(opt),
		inspector: asynq.NewInspector(opt),
		redis:     redis.NewClient(rOpt),
		registry:  registry,
	}, nil
}

func (o *Orchestrator) Close() error {
	if err := o.client.Close(); err != nil {
		return err
	}
	return o.redis.Close()
}

// HealthCheck returns a function suitable for the readiness probe.
func (o *Orchestrator) HealthCheck() func(ctx context.Context) error {
	return func(ctx context.Context) error {
		return o.redis.Ping(ctx).Err()
	}
}

// Enqueue persists blob buffers to Redis and submits the task.
func (o *Orchestrator) Enqueue(ctx context.Context, p *Payload) (string, error) {
	bufs := p.takeBuffers()
	for i, buf := range bufs {
		key := fmt.Sprintf("owui-cee:blob:%s", uuid.NewString())
		if err := o.redis.Set(ctx, key, buf.Bytes(), o.cfg.Retention).Err(); err != nil {
			return "", fmt.Errorf("persist blob: %w", err)
		}
		p.BlobKeys[i].Key = key
	}
	body, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	t := asynq.NewTask(TypeConvert, body)
	info, err := o.client.EnqueueContext(ctx, t,
		asynq.MaxRetry(o.cfg.Retry),
		asynq.Retention(o.cfg.Retention),
		asynq.Timeout(o.cfg.Retention),
	)
	if err != nil {
		return "", err
	}
	return info.ID, nil
}

// StatusInfo is the public shape returned to /v1/status/poll.
type StatusInfo struct {
	TaskID     string `json:"task_id"`
	TaskStatus string `json:"task_status"`
	Queue      string `json:"queue"`
	Retried    int    `json:"retried"`
	MaxRetry   int    `json:"max_retry"`
	LastError  string `json:"last_error,omitempty"`
}

func (o *Orchestrator) Status(ctx context.Context, id string) (*StatusInfo, error) {
	queues, err := o.inspector.Queues()
	if err != nil {
		return nil, err
	}
	for _, q := range queues {
		info, err := o.inspector.GetTaskInfo(q, id)
		if err == nil && info != nil {
			return &StatusInfo{
				TaskID:     id,
				TaskStatus: mapState(info.State),
				Queue:      info.Queue,
				Retried:    info.Retried,
				MaxRetry:   info.MaxRetry,
				LastError:  info.LastErr,
			}, nil
		}
	}
	return nil, ErrNotFound
}

func mapState(s asynq.TaskState) string {
	switch s {
	case asynq.TaskStateActive:
		return "started"
	case asynq.TaskStatePending, asynq.TaskStateScheduled, asynq.TaskStateAggregating:
		return "pending"
	case asynq.TaskStateRetry:
		return "pending"
	case asynq.TaskStateArchived, asynq.TaskStateCompleted:
		return "success"
	default:
		return "unknown"
	}
}

func (o *Orchestrator) Result(ctx context.Context, id string) (io.ReadCloser, string, error) {
	key := resultKey(id)
	b, err := o.redis.Get(ctx, key).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, "", ErrNotFound
		}
		return nil, "", err
	}
	ct, _ := o.redis.Get(ctx, key+":ct").Result()
	return io.NopCloser(bytes.NewReader(b)), ct, nil
}

func resultKey(id string) string { return "owui-cee:result:" + id }

// SaveResult is invoked by the worker once the engine returns.
func (o *Orchestrator) SaveResult(ctx context.Context, id, contentType string, body []byte) error {
	ttl := o.cfg.ResultTTL
	if ttl <= 0 {
		ttl = time.Hour
	}
	if err := o.redis.Set(ctx, resultKey(id), body, ttl).Err(); err != nil {
		return err
	}
	return o.redis.Set(ctx, resultKey(id)+":ct", contentType, ttl).Err()
}

// LoadBlob retrieves a blob persisted at Enqueue time.
func (o *Orchestrator) LoadBlob(ctx context.Context, key string) ([]byte, error) {
	b, err := o.redis.Get(ctx, key).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return b, nil
}
