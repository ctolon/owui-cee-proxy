package tasks

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"

	"github.com/ctolon/owui-cee-proxy/internal/config"
)

// newOrchestratorWithMiniredis spins a miniredis server and returns a
// fully-wired Orchestrator pointed at it. Lifts the long-standing
// `t.Skip("requires Redis")` ban from the lifecycle-shaped tests so
// Enqueue → Status → SaveResult → Result can be exercised end-to-end
// in unit mode.
func newOrchestratorWithMiniredis(t *testing.T) (*Orchestrator, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	cfg := config.TasksConfig{
		Enabled:       true,
		RedisURL:      config.Secret("redis://" + mr.Addr()),
		Retention:     5 * time.Minute,
		ResultTTL:     5 * time.Minute,
		TaskTimeout:   30 * time.Second,
		MaxBlobBytes:  1 << 20,
		MaxTotalBytes: 4 << 20,
		Retry:         1,
	}
	orch, err := NewOrchestrator(cfg, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = orch.Close() })
	return orch, mr
}

func TestMintToken_LengthAndUniqueness(t *testing.T) {
	t.Parallel()
	a, err := mintToken()
	if err != nil {
		t.Fatalf("mintToken: %v", err)
	}
	b, err := mintToken()
	if err != nil {
		t.Fatalf("mintToken: %v", err)
	}
	if len(a) != 64 {
		t.Fatalf("expected 64 hex chars, got %d", len(a))
	}
	if _, err := hex.DecodeString(a); err != nil {
		t.Fatalf("token is not hex: %v", err)
	}
	if a == b {
		t.Fatalf("two consecutive tokens collided: %s", a)
	}
}

func TestKeyFromAPIKey_Deterministic(t *testing.T) {
	t.Parallel()
	a := keyFromAPIKey("foo")
	b := keyFromAPIKey("foo")
	if a != b {
		t.Fatalf("non-deterministic: %s vs %s", a, b)
	}
	if len(a) != 64 {
		t.Fatalf("expected 64 hex chars, got %d", len(a))
	}
	if _, err := hex.DecodeString(a); err != nil {
		t.Fatalf("fingerprint is not hex: %v", err)
	}
	if c := keyFromAPIKey("bar"); c == a {
		t.Fatalf("expected different fingerprints for different keys")
	}
	if keyFromAPIKey("") != "" {
		t.Fatalf("empty input must yield empty fingerprint")
	}
}

func TestValidateBlobLimits(t *testing.T) {
	t.Parallel()
	tcs := []struct {
		name       string
		refs       []BlobRef
		maxBlob    int64
		maxTotal   int64
		wantErrSub string
	}{
		{
			name:    "ok within both caps",
			refs:    []BlobRef{{Size: 50}, {Size: 50}},
			maxBlob: 100, maxTotal: 200,
		},
		{
			name:       "single blob exceeds per-blob cap",
			refs:       []BlobRef{{Filename: "big.pdf", Size: 150}},
			maxBlob:    100,
			maxTotal:   1000,
			wantErrSub: "exceeds max_blob_bytes",
		},
		{
			name:       "aggregate exceeds total cap",
			refs:       []BlobRef{{Size: 60}, {Size: 60}},
			maxBlob:    100,
			maxTotal:   100,
			wantErrSub: "exceeds max_total_bytes",
		},
		{
			name:    "zero caps disable limits",
			refs:    []BlobRef{{Size: 1 << 30}},
			maxBlob: 0, maxTotal: 0,
		},
		{
			name:       "negative size rejected",
			refs:       []BlobRef{{Filename: "x", Size: -1}},
			maxBlob:    100,
			maxTotal:   100,
			wantErrSub: "size unknown",
		},
	}
	for _, tc := range tcs {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := &Payload{BlobKeys: tc.refs}
			err := validateBlobLimits(p, tc.maxBlob, tc.maxTotal)
			if tc.wantErrSub == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErrSub)
			}
			if !strings.Contains(err.Error(), tc.wantErrSub) {
				t.Fatalf("error %q missing substring %q", err.Error(), tc.wantErrSub)
			}
		})
	}
}

// TestOrchestrator_EnqueueSaveResultRoundtrip closes the C-14 unit-
// coverage gap for the orchestrator's persistence surface. With a
// miniredis backend we exercise the path Enqueue → resolveToken →
// SaveResult → Result end-to-end without standing up a real Redis
// container.
//
// Status() is NOT part of this assertion because the underlying
// asynq.Inspector walks Redis data structures (LRANGE / ZRANGEBYSCORE
// against asynq's internal queue keys) that miniredis can emulate but
// not perfectly model when asynq writes via its own Lua scripts. That
// path stays covered by the integration suite; here we pin the
// Orchestrator's Redis-direct lifecycle.
func TestOrchestrator_EnqueueSaveResultRoundtrip(t *testing.T) {
	t.Parallel()
	orch, _ := newOrchestratorWithMiniredis(t)

	ctx := context.Background()
	p := &Payload{
		Engine:    "main-docling",
		Facade:    "docling",
		RequestID: "req-1",
		Options:   map[string]string{"output_format": "markdown"},
	}

	token, err := orch.Enqueue(ctx, p, "caller-key-A")
	require.NoError(t, err, "miniredis-backed Enqueue must succeed")
	require.NotEmpty(t, token, "Enqueue returns a minted token")

	// Pull the asynq task ID from the token binding we just wrote.
	// resolveToken is the in-package helper Status/Result use; the
	// happy path round-trips correctly via the Redis-direct calls.
	id, err := orch.resolveToken(ctx, token, "caller-key-A")
	require.NoError(t, err)
	require.NotEmpty(t, id)

	// Simulate the worker writing the final result. The retrieval
	// path then yields the body + content type via the result
	// store (pure redis Set/Get — no asynq inspector involvement).
	require.NoError(t, orch.SaveResult(ctx, id, "application/json",
		[]byte(`{"document":{"md_content":"ok"}}`)))

	body, ct, err := orch.Result(ctx, token, "caller-key-A")
	require.NoError(t, err)
	defer body.Close()
	require.Equal(t, "application/json", ct)
	out, err := io.ReadAll(body)
	require.NoError(t, err)
	require.Contains(t, string(out), `"md_content":"ok"`)
}

// TestOrchestrator_TokenBindingRejectsCrossTenant pins the load-
// bearing security invariant for the async path: a task token MUST
// only resolve when paired with the SAME caller API key that
// produced it. Without this, anyone holding a leaked token could
// poll any tenant's task and fetch its result body.
//
// The previous test suite skipped this on the assumption that the
// integration suite covered it — F1 from the architecture review
// proved that `Async.APIKeyHeader` was unwired in production
// (token binding collapsed to empty fingerprint, no cross-tenant
// barrier). Pin it at unit level now.
func TestOrchestrator_TokenBindingRejectsCrossTenant(t *testing.T) {
	t.Parallel()
	orch, _ := newOrchestratorWithMiniredis(t)

	ctx := context.Background()
	p := &Payload{Engine: "main-docling", Facade: "docling", RequestID: "tenant-A-req"}
	token, err := orch.Enqueue(ctx, p, "tenant-A")
	require.NoError(t, err)

	// Same caller: success.
	_, err = orch.Status(ctx, token, "tenant-A")
	require.NoError(t, err)

	// Different caller: must surface ErrNotFound (not an auth-level
	// error — the orchestrator treats the lookup as if the token
	// doesn't exist, denying any oracle on whether a token is
	// valid-but-for-someone-else).
	_, err = orch.Status(ctx, token, "tenant-B")
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrNotFound),
		"cross-tenant Status MUST return ErrNotFound, got %v", err)

	// Same for Result.
	_, _, err = orch.Result(ctx, token, "tenant-B")
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrNotFound))

	// And an empty caller (the F1 footgun): also rejected.
	_, err = orch.Status(ctx, token, "")
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrNotFound))
}

// TestOrchestrator_IdempotencyResolveAndRecord pins the C-20
// Idempotency-Key contract:
//   - first call with a given (apiKey, idem) records the token and
//     resolves it on subsequent reads;
//   - empty idem is a no-op (caller intentionally opted out);
//   - cross-tenant: same idem-key under a different apiKey returns
//     a distinct lookup (no collision);
//   - SETNX race: if a second RecordIdempotent fires with a
//     different token for the same (apiKey, idem), the FIRST token
//     wins and the loser sees the winner's value.
func TestOrchestrator_IdempotencyResolveAndRecord(t *testing.T) {
	t.Parallel()
	orch, _ := newOrchestratorWithMiniredis(t)
	ctx := context.Background()

	// Empty idem-key short-circuits without touching Redis.
	stored, found, err := orch.ResolveIdempotent(ctx, "", "tenant-A")
	require.NoError(t, err)
	require.False(t, found)
	require.Empty(t, stored)

	// Miss on first lookup → not found.
	_, found, err = orch.ResolveIdempotent(ctx, "client-key-1", "tenant-A")
	require.NoError(t, err)
	require.False(t, found)

	// Record the (tenant-A, client-key-1) → token-A binding.
	tok, err := orch.RecordIdempotent(ctx, "client-key-1", "tenant-A", "token-A")
	require.NoError(t, err)
	require.Equal(t, "token-A", tok)

	// Same lookup now resolves to token-A.
	stored, found, err = orch.ResolveIdempotent(ctx, "client-key-1", "tenant-A")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "token-A", stored)

	// Race: a second Record with a different token for the SAME
	// (tenant, key) returns the FIRST token. The second caller now
	// shares the same task as the first.
	tok, err = orch.RecordIdempotent(ctx, "client-key-1", "tenant-A", "token-B")
	require.NoError(t, err)
	require.Equal(t, "token-A", tok, "SETNX winner-token MUST be returned to the loser")

	// Cross-tenant: same idem-key under tenant-B is independent.
	_, found, err = orch.ResolveIdempotent(ctx, "client-key-1", "tenant-B")
	require.NoError(t, err)
	require.False(t, found, "(tenant-A, idem) MUST NOT shadow (tenant-B, idem)")

	tok, err = orch.RecordIdempotent(ctx, "client-key-1", "tenant-B", "token-B")
	require.NoError(t, err)
	require.Equal(t, "token-B", tok)
}

// TestOrchestrator_FingerprintPepperHMAC pins C-28: when the
// orchestrator carries a non-empty pepper, the per-caller fingerprint
// is HMAC-SHA-256(pepper, apiKey) — NOT plain SHA-256. An attacker
// who exfils Redis but doesn't steal the pepper cannot run an
// offline rainbow-table against the fingerprint to recover the key.
//
// Empty pepper falls back to plain SHA-256 for backward-compat with
// existing deployments.
func TestOrchestrator_FingerprintPepperHMAC(t *testing.T) {
	t.Parallel()
	orch, _ := newOrchestratorWithMiniredis(t)

	// Default (no pepper) — plain SHA-256.
	require.Equal(t, keyFromAPIKey("tenant-A"), orch.keyFromAPIKey("tenant-A"))

	// With pepper — HMAC differs from plain SHA-256.
	orch.WithFingerprintPepper([]byte("super-secret-pepper"))
	pepperedA := orch.keyFromAPIKey("tenant-A")
	require.NotEqual(t, keyFromAPIKey("tenant-A"), pepperedA,
		"peppered fingerprint MUST diverge from plain SHA-256")
	require.Equal(t, 64, len(pepperedA), "HMAC-SHA-256 hex length")

	// Determinism — same key + same pepper → same fingerprint.
	require.Equal(t, pepperedA, orch.keyFromAPIKey("tenant-A"))

	// Cross-tenant isolation preserved under HMAC.
	require.NotEqual(t, pepperedA, orch.keyFromAPIKey("tenant-B"))

	// Empty input still returns empty fingerprint (no binding).
	require.Empty(t, orch.keyFromAPIKey(""))
}
