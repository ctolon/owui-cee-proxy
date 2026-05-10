package tasks

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
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
	cfg       config.TasksConfig
	client    *asynq.Client
	inspector *asynq.Inspector
	redis     *redis.Client
	registry  engine.Registry
}

func NewOrchestrator(cfg config.TasksConfig, registry engine.Registry) (*Orchestrator, error) {
	if !cfg.Enabled {
		return nil, errors.New("tasks: not enabled")
	}
	if cfg.RedisURL == "" {
		return nil, errors.New("tasks: REDIS_URL is required")
	}
	redisURL := string(cfg.RedisURL)
	opt, err := asynq.ParseRedisURI(redisURL)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}
	rOpt, err := redis.ParseURL(redisURL)
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

// Config returns the task configuration (read-only, by value).
func (o *Orchestrator) Config() config.TasksConfig { return o.cfg }

// HealthCheck returns a function suitable for the readiness probe.
func (o *Orchestrator) HealthCheck() func(ctx context.Context) error {
	return func(ctx context.Context) error {
		return o.redis.Ping(ctx).Err()
	}
}

// Redis key helpers.
func tokenKey(token string) string  { return "owui-cee:token:" + token }
func queueKey(taskID string) string { return "owui-cee:queue:" + taskID }
func resultKey(id string) string    { return "owui-cee:result:" + id }

// mintToken returns a 32-byte token rendered as 64 hex chars.
func mintToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// keyFromAPIKey returns a deterministic 32-byte hex (64 char)
// fingerprint for the given API key. Used to bind a task token to the
// caller. Empty key → empty fingerprint (no binding).
func keyFromAPIKey(apiKey string) string {
	if apiKey == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(apiKey))
	return hex.EncodeToString(sum[:])
}

// validateBlobLimits enforces tasks.max_blob_bytes (per file) and
// tasks.max_total_bytes (aggregate) on the parsed payload. Caps of 0
// disable the corresponding check.
func validateBlobLimits(p *Payload, maxBlob, maxTotal int64) error {
	if p == nil {
		return nil
	}
	var total int64
	for _, ref := range p.BlobKeys {
		if ref.Size < 0 {
			return fmt.Errorf("blob %q: size unknown", ref.Filename)
		}
		if maxBlob > 0 && ref.Size > maxBlob {
			return fmt.Errorf("blob %q: size %d exceeds max_blob_bytes %d", ref.Filename, ref.Size, maxBlob)
		}
		total += ref.Size
		if maxTotal > 0 && total > maxTotal {
			return fmt.Errorf("aggregate blob size %d exceeds max_total_bytes %d", total, maxTotal)
		}
	}
	return nil
}

// tokenBinding is the JSON record we persist under tokenKey to map a
// caller-visible token to the internal asynq task ID. Bound to the
// caller's API-key fingerprint when provided.
type tokenBinding struct {
	TaskID       string `json:"task_id"`
	APIKeyFinger string `json:"key_fp,omitempty"`
}

// Enqueue persists blob buffers to Redis and submits the task. The
// caller-visible task ID returned is an opaque token; internal asynq
// IDs never leak. apiKey is the proxy API key from the caller (may be
// empty if security.require_api_key is false); when non-empty the
// token is bound to its fingerprint.
func (o *Orchestrator) Enqueue(ctx context.Context, p *Payload, apiKey string) (string, error) {
	if err := validateBlobLimits(p, o.cfg.MaxBlobBytes, o.cfg.MaxTotalBytes); err != nil {
		return "", err
	}
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
	timeout := o.cfg.TaskTimeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	t := asynq.NewTask(TypeConvert, body)
	info, err := o.client.EnqueueContext(ctx, t,
		asynq.MaxRetry(o.cfg.Retry),
		asynq.Retention(o.cfg.Retention),
		asynq.Timeout(timeout),
	)
	if err != nil {
		return "", err
	}

	// Mint opaque token, bind to internal asynq ID + caller fingerprint.
	token, err := mintToken()
	if err != nil {
		return "", fmt.Errorf("mint token: %w", err)
	}
	binding := tokenBinding{TaskID: info.ID, APIKeyFinger: keyFromAPIKey(apiKey)}
	bb, err := json.Marshal(binding)
	if err != nil {
		return "", err
	}
	ttl := o.cfg.ResultTTL
	if ttl <= 0 {
		ttl = time.Hour
	}
	if err := o.redis.Set(ctx, tokenKey(token), bb, ttl).Err(); err != nil {
		return "", fmt.Errorf("persist token: %w", err)
	}
	// Persist task_id → queue mapping so Status() can target a single queue.
	if err := o.redis.Set(ctx, queueKey(info.ID), info.Queue, o.cfg.Retention).Err(); err != nil {
		return "", fmt.Errorf("persist queue mapping: %w", err)
	}
	return token, nil
}

// resolveToken returns the internal asynq task ID bound to the caller-
// visible opaque token. When apiKey is non-empty the binding's
// fingerprint must match; otherwise we return ErrNotFound to avoid
// leaking the token's existence to other callers.
func (o *Orchestrator) resolveToken(ctx context.Context, token, apiKey string) (string, error) {
	b, err := o.redis.Get(ctx, tokenKey(token)).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return "", ErrNotFound
		}
		return "", err
	}
	var binding tokenBinding
	if err := json.Unmarshal(b, &binding); err != nil {
		return "", err
	}
	if binding.APIKeyFinger != "" {
		if keyFromAPIKey(apiKey) != binding.APIKeyFinger {
			return "", ErrNotFound
		}
	}
	return binding.TaskID, nil
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

// Status looks up a task by caller-visible token. apiKey is used to
// gate cross-key access when require_api_key is enabled.
func (o *Orchestrator) Status(ctx context.Context, token, apiKey string) (*StatusInfo, error) {
	id, err := o.resolveToken(ctx, token, apiKey)
	if err != nil {
		return nil, err
	}
	// Fast path: task_id → queue mapping was persisted at enqueue.
	queue, qerr := o.redis.Get(ctx, queueKey(id)).Result()
	if qerr == nil && queue != "" {
		if info, err := o.inspector.GetTaskInfo(queue, id); err == nil && info != nil {
			return &StatusInfo{
				TaskID:     token,
				TaskStatus: mapState(info.State),
				Queue:      info.Queue,
				Retried:    info.Retried,
				MaxRetry:   info.MaxRetry,
				LastError:  info.LastErr,
			}, nil
		}
	}
	// Fallback: scan all queues (eg. mapping TTL elapsed).
	queues, err := o.inspector.Queues()
	if err != nil {
		return nil, err
	}
	for _, q := range queues {
		info, err := o.inspector.GetTaskInfo(q, id)
		if err == nil && info != nil {
			return &StatusInfo{
				TaskID:     token,
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

// Result resolves the caller-visible token to the internal task ID
// then returns the stored body + content-type. Returns ErrNotFound if
// the token is unknown, bound to a different API key, or the result
// is not yet present.
func (o *Orchestrator) Result(ctx context.Context, token, apiKey string) (io.ReadCloser, string, error) {
	id, err := o.resolveToken(ctx, token, apiKey)
	if err != nil {
		return nil, "", err
	}
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
