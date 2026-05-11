package tasks

import (
	"bytes"
	"context"
	"crypto/hmac"
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
	"github.com/rs/zerolog"

	"github.com/ctolon/owui-cee-proxy/internal/config"
	"github.com/ctolon/owui-cee-proxy/internal/engine"
)

var ErrNotFound = errors.New("tasks: not found")

// LifecycleRecorder is the observability seam for the async path.
// Nil-safe (Orchestrator/Worker default to a Nop recorder when not
// wired) so unit tests don't need to know about metrics. Production
// wires this to the TaskLifecycle counter.
type LifecycleRecorder interface {
	RecordTaskLifecycle(stage, outcome string)
}

type nopLifecycleRecorder struct{}

func (nopLifecycleRecorder) RecordTaskLifecycle(string, string) {}

// QueueDepthRecorder sets the WorkerQueueDepth gauge once per poll.
// Nil-safe.
type QueueDepthRecorder interface {
	SetQueueDepth(queue string, depth int)
}

type nopQueueDepthRecorder struct{}

func (nopQueueDepthRecorder) SetQueueDepth(string, int) {}

// Orchestrator is the API-side handle on the asynq client and Redis
// blob store. The worker side runs in the same binary but on a
// different lifecycle (NewWorker).
type Orchestrator struct {
	cfg       config.TasksConfig
	client    *asynq.Client
	inspector *asynq.Inspector
	redis     *redis.Client
	registry  engine.Registry
	logger    zerolog.Logger
	lifecycle LifecycleRecorder
	queueDep  QueueDepthRecorder
	// fingerprintPepper is the per-process secret HMAC'd into
	// caller-API-key fingerprints. When non-empty, fingerprints are
	// HMAC-SHA-256(pepper, apiKey); a Redis exfil can no longer
	// recover the key via offline rainbow-tables against the
	// fingerprint (C-28 from REVIEW-FAANG.md). Empty → fallback to
	// plain SHA-256 for backward compat with existing
	// deployments / unit-test fixtures.
	fingerprintPepper []byte
}

// WithFingerprintPepper installs the HMAC pepper used by
// keyFromAPIKey. Composition root passes the resolved
// `security.proxy_api_key_fingerprint_pepper_env` value here at
// startup. Returns the orchestrator for the same chaining shape as
// WithObservability.
func (o *Orchestrator) WithFingerprintPepper(pepper []byte) *Orchestrator {
	if len(pepper) > 0 {
		cp := make([]byte, len(pepper))
		copy(cp, pepper)
		o.fingerprintPepper = cp
	}
	return o
}

// WithObservability wires the structured logger + metric recorders.
// Returns the same orchestrator so app.go can chain: NewOrchestrator
// then WithObservability. Nil arguments fall back to the safe Nops.
func (o *Orchestrator) WithObservability(logger zerolog.Logger, lc LifecycleRecorder, qd QueueDepthRecorder) *Orchestrator {
	o.logger = logger
	if lc == nil {
		lc = nopLifecycleRecorder{}
	}
	if qd == nil {
		qd = nopQueueDepthRecorder{}
	}
	o.lifecycle = lc
	o.queueDep = qd
	return o
}

// Inspector exposes the asynq inspector to the queue-depth poller
// goroutine the composition root spawns. Returned read-only.
func (o *Orchestrator) Inspector() *asynq.Inspector { return o.inspector }

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
		lifecycle: nopLifecycleRecorder{},
		queueDep:  nopQueueDepthRecorder{},
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
func tokenKey(token string) string { return "owui-cee:token:" + token }

// idemKey is the Redis key used to memoise an idempotent submit. The
// (caller-fingerprint, idempotency-key) tuple is hashed so neither
// value lands raw in Redis — a dump of the keyspace leaks no
// credential material and no caller-chosen identifiers.
func idemKey(apiKeyFP, idemKey string) string {
	sum := sha256.Sum256([]byte(idemKey))
	return "owui-cee:idem:" + apiKeyFP + ":" + hex.EncodeToString(sum[:])
}
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
// fingerprint for the given API key. Used to bind a task token to
// the caller. Empty key → empty fingerprint (no binding).
//
// LEGACY: plain SHA-256. Kept for backward-compat with existing
// deployments / unit-test fixtures. New code paths SHOULD route
// through o.keyFromAPIKey (the method) which honours the
// fingerprintPepper when configured — see C-28.
func keyFromAPIKey(apiKey string) string {
	if apiKey == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(apiKey))
	return hex.EncodeToString(sum[:])
}

// keyFromAPIKey is the orchestrator's per-instance fingerprint
// helper. When fingerprintPepper is set, returns
// HMAC-SHA-256(pepper, apiKey) hex — a Redis exfil + offline
// rainbow-table cannot recover the original key without ALSO
// stealing the pepper from the proxy process. Empty pepper falls
// back to the package-level keyFromAPIKey (plain SHA-256) so
// existing deployments keep working.
func (o *Orchestrator) keyFromAPIKey(apiKey string) string {
	if apiKey == "" {
		return ""
	}
	if len(o.fingerprintPepper) == 0 {
		// No pepper configured — fall back to the package-level
		// plain SHA-256 helper. Calling the method here would
		// recurse infinitely.
		return keyFromAPIKey(apiKey)
	}
	mac := hmac.New(sha256.New, o.fingerprintPepper)
	mac.Write([]byte(apiKey))
	return hex.EncodeToString(mac.Sum(nil))
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

	// Pipeline the blob SETs into a single Redis round-trip instead
	// of one RTT per blob (N blobs → 1 RTT not N). Pure pipeline (no
	// MULTI/EXEC) — we don't need atomicity across the batch, just
	// the round-trip savings on slow / high-RTT Redis links.
	// (C-23 from REVIEW-FAANG.md: async submit was doing 4 sequential
	// RTTs inside the request goroutine; slow Redis blocked every
	// async submit.)
	bufs := p.takeBuffers()
	if len(bufs) > 0 {
		pipe := o.redis.Pipeline()
		blobKeys := make([]string, len(bufs))
		for i, buf := range bufs {
			blobKeys[i] = fmt.Sprintf("owui-cee:blob:%s", uuid.NewString())
			pipe.Set(ctx, blobKeys[i], buf.Bytes(), o.cfg.Retention)
		}
		if _, err := pipe.Exec(ctx); err != nil {
			return "", fmt.Errorf("persist blobs: %w", err)
		}
		for i, k := range blobKeys {
			p.BlobKeys[i].Key = k
		}
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
	info, err := o.client.EnqueueContext(
		ctx, t,
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
	binding := tokenBinding{TaskID: info.ID, APIKeyFinger: o.keyFromAPIKey(apiKey)}
	bb, err := json.Marshal(binding)
	if err != nil {
		return "", err
	}
	ttl := o.cfg.ResultTTL
	if ttl <= 0 {
		ttl = time.Hour
	}
	// Pipeline the two post-Enqueue SETs (token binding + queue
	// mapping) into ONE round-trip. Pre-fix this was two sequential
	// RTTs after the asynq Enqueue's own RTT.
	pipe := o.redis.Pipeline()
	pipe.Set(ctx, tokenKey(token), bb, ttl)
	pipe.Set(ctx, queueKey(info.ID), info.Queue, o.cfg.Retention)
	if _, err := pipe.Exec(ctx); err != nil {
		return "", fmt.Errorf("persist token+queue: %w", err)
	}

	o.lifecycle.RecordTaskLifecycle("enqueued", "success")
	o.logger.Info().
		Str("event", "task_enqueued").
		Str("task_id", info.ID).
		Str("token", token).
		Str("engine", p.Engine).
		Str("queue", info.Queue).
		Str("request_id", p.RequestID).
		Int("file_count", len(p.BlobKeys)).
		Msg("async task enqueued")

	return token, nil
}

// PollQueueDepth fires once per period (driven by the composition
// root's goroutine) and sets the WorkerQueueDepth gauge for every
// known queue. Errors from the inspector are logged at debug level
// (we don't fail the proxy if Redis hiccups for a poll cycle).
func (o *Orchestrator) PollQueueDepth(ctx context.Context) {
	queues, err := o.inspector.Queues()
	if err != nil {
		o.logger.Debug().Err(err).Msg("queue-depth poll: list queues failed")
		return
	}
	for _, q := range queues {
		info, err := o.inspector.GetQueueInfo(q)
		if err != nil {
			o.logger.Debug().Err(err).Str("queue", q).Msg("queue-depth poll: queue info failed")
			continue
		}
		// Depth = active + pending + scheduled + retry + aggregating.
		// This is the canonical "still-doing-work" headline number.
		depth := info.Active + info.Pending + info.Scheduled + info.Retry + info.Aggregating
		o.queueDep.SetQueueDepth(q, depth)
	}
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
		if o.keyFromAPIKey(apiKey) != binding.APIKeyFinger {
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

// ResolveIdempotent looks up a previously-recorded submission keyed
// by (apiKey fingerprint, Idempotency-Key). Returns the stored token
// when the key matches, or empty/false when no prior submission was
// recorded.
//
// Treat empty `idem` as "no idempotency-key sent" — the caller skips
// the dedupe entirely. Empty `apiKey` is also tolerated (anonymous
// public engines) but produces a fingerprint of "" which is shared
// across all anonymous callers — operators running without
// require_api_key SHOULD NOT rely on idempotency dedupe in
// multi-tenant scenarios.
func (o *Orchestrator) ResolveIdempotent(ctx context.Context, idem, apiKey string) (string, bool, error) {
	if idem == "" {
		return "", false, nil
	}
	v, err := o.redis.Get(ctx, idemKey(o.keyFromAPIKey(apiKey), idem)).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("idem lookup: %w", err)
	}
	return v, true, nil
}

// RecordIdempotent atomically reserves the (apiKey, idem) tuple to
// the issued task token. SETNX semantics: if a concurrent submit
// already recorded a different token, we return that token instead
// so the second caller sees the SAME task as the first. TTL equals
// ResultTTL so the dedupe window matches the period during which a
// client can still poll/fetch the original result.
//
// Returns the EFFECTIVE token: usually `token`, but the prior token
// when a race lost the SETNX. Callers MUST honour the returned
// value (use it as the response payload).
func (o *Orchestrator) RecordIdempotent(ctx context.Context, idem, apiKey, token string) (string, error) {
	if idem == "" {
		return token, nil
	}
	ttl := o.cfg.ResultTTL
	if ttl <= 0 {
		ttl = time.Hour
	}
	key := idemKey(o.keyFromAPIKey(apiKey), idem)
	ok, err := o.redis.SetNX(ctx, key, token, ttl).Result()
	if err != nil {
		return token, fmt.Errorf("idem record: %w", err)
	}
	if ok {
		return token, nil
	}
	// SETNX raced — read the winner.
	v, err := o.redis.Get(ctx, key).Result()
	if err != nil {
		return token, fmt.Errorf("idem read-after-lost-race: %w", err)
	}
	return v, nil
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
