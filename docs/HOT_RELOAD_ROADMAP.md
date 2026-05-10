# Hot reload roadmap

A plan to take owui-cee-proxy from "stop the binary, start a new
one" to nginx-style "send SIGHUP, the new config takes effect with
zero dropped connections, zero restart, zero leaked goroutines".

This is a separate workstream from the security/perf review and
should land after the high-severity items in `docs/REVIEW.md` so we
don't bake regressions into a more complex runtime.

---

## 1. Goals (in scope)

1. **Live YAML reload** on signal or explicit IPC. Operators edit
   the file, send `SIGHUP`, and within ~1 s the proxy is honouring
   the new values.
2. **Zero dropped connections** during reload. In-flight requests
   keep their old engine wiring; new requests use the new wiring.
3. **No new file descriptors / process exec.** Single-process design
   stays. (Some tools — Caddy, nginx, Tableflip — solve this with a
   parent/child fd handoff. We deliberately avoid that complication
   for now.)
4. **Reload reports outcome** via JSON log line + (optionally)
   admin endpoint. Bad config is rejected; the running config is
   untouched.
5. **fsnotify watcher** as an alternative trigger to `SIGHUP` so
   GitOps tools that mount a ConfigMap as a file get atomic updates
   for free.
6. **Idempotent**: applying the same config twice is a no-op.

## 2. Non-goals

- Listener replacement (cert rotation alone is enough; full bind
  swap requires SO_REUSEPORT + handoff).
- Cross-process reload via Tableflip / facebookgo/grace.
- Online migration of in-flight asynq tasks.
- Hot reload of `server.listen` (changing the bind address requires
  binary restart; that's documented).

---

## 3. What can change without a restart

| Concern                              | Hot-reloadable? | Notes                                                                                                  |
|--------------------------------------|-----------------|--------------------------------------------------------------------------------------------------------|
| `routing.default_cee_engine`          | ✅              | Atomic pointer swap on the registry.                                                                    |
| `engines.<n>.enable`                  | ✅              | New registry built; old engine drained.                                                                 |
| `engines.<n>.url`                     | ✅              | Triggers a new `*http.Transport` / new client; old transport drained.                                  |
| `engines.<n>.api_key_env` value       | ✅              | Re-resolved from env (env itself can be re-read on `SIGHUP`).                                           |
| `engines.<n>.mime_types`              | ✅              | New registry pattern set.                                                                               |
| `engines.<n>.request_timeout`         | ✅              | Stored on the new client.                                                                               |
| `engines.<n>.http.*` tuning           | ✅              | New `*http.Transport`.                                                                                  |
| `engines.<n>.breaker.*`               | ✅              | New `gobreaker.CircuitBreaker`. Caveat: breaker COUNTS reset on swap — document.                       |
| `engines.<n>.rate_limit.*`            | ✅              | New `rate.Limiter`. Tokens reset.                                                                       |
| `routing.passthrough.*_prefix`        | ⚠️ partial      | Mount path lives in the chi router; we'll need to rebuild the router (see §6).                          |
| `security.proxy_api_keys_env`         | ✅              | Hot-loaded into the apikey middleware.                                                                  |
| `security.require_api_key`            | ✅              | Same.                                                                                                  |
| `ratelimit_global.*`                  | ✅              | New `rate.Limiter`.                                                                                    |
| `tasks.enabled`                       | ⚠️ partial      | Turning ON requires starting an asynq Server; turning OFF requires draining + stopping it.            |
| `tasks.queue_concurrency`             | ⚠️              | asynq exposes `Server.Stop()` + replace.                                                                |
| `tasks.retention/retry/result_ttl`    | ✅              | Stored on the orchestrator.                                                                            |
| `observability.log.level`/`format`    | ✅              | zerolog level swap; format change rebuilds the logger.                                                 |
| `observability.metrics.*`             | ⚠️              | Path change requires re-mounting the route handler.                                                    |
| `observability.tracing.*`             | ⚠️              | OTLP exporter swap means closing the old `TracerProvider` and registering a new one.                    |
| `server.tls.cert_file/key_file`       | ✅ (cert rotation) | `tls.Config.GetCertificate` callback re-reads files on every handshake.                                |
| `server.read_timeout` etc.            | ❌              | These are baked into `http.Server` at construction. Restart the server.                                 |
| `server.listen`                       | ❌              | Bind change requires re-listen → process restart.                                                       |

---

## 4. Architecture

### 4.1 The atomic config pointer

```go
// internal/runtime/runtime.go (new package)
type Snapshot struct {
    Cfg          *config.Config
    Registry     engine.Registry
    GlobalLimit  *rate.Limiter
    APIKeyAllow  []string
    Logger       zerolog.Logger
    Metrics      *observability.Metrics  // unchanged across reload
    Orchestrator *tasks.Orchestrator     // may be nil (or be (re)created)
    Worker       *tasks.Worker           // ditto
    TraceShutdown func(context.Context) error
}

// Singleton owned by the application; readers fetch the snapshot
// at the start of every request handler.
var current atomic.Pointer[Snapshot]

func Current() *Snapshot                  { return current.Load() }
func Swap(next *Snapshot) (*Snapshot, error) {
    prev := current.Swap(next)
    return prev, nil
}
```

Every middleware and handler that today closes over a `*config.Config`
or a registry instance instead reads `runtime.Current()` once per
request.

The atomic pointer is wait-free for readers — the same property the
existing immutable `staticRegistry` already relies on.

### 4.2 Reload pipeline

```
SIGHUP / fsnotify / POST /admin/reload (auth-gated)
  └─▶ runtime.Reload()
        ├─ load + validate the new YAML (config.Load)
        ├─ build a new Snapshot (registry, clients, breakers, limiters)
        │     │
        │     ├─ engines unchanged → reuse the existing adapter and
        │     │   transport (engine fingerprint matches)
        │     ├─ engines changed   → construct a NEW adapter; mark
        │     │   the OLD adapter for graceful retirement
        │     └─ engines disabled  → mark the OLD adapter for retirement
        │
        ├─ atomic.Pointer[Snapshot].Swap(new)
        │
        └─ retire the old: cancel its in-flight ctx after grace, then
           CloseIdleConnections() on its transport, then drop reference
```

The "engine fingerprint" is the hash of `(URL, http.*, breaker.*,
rate_limit.*, api_key_env, mime_types, forward_options)`. Reusing
unchanged engines preserves their warm idle pool and breaker state.

### 4.3 Trigger surface

| Trigger                                        | Default | Notes                                                                       |
|------------------------------------------------|---------|------------------------------------------------------------------------------|
| `SIGHUP`                                       | on      | Idiomatic Unix; matches nginx and Caddy.                                    |
| `fsnotify` watcher on `OWUI_PROXY_CONFIG`      | on      | Safety: debounce 1 s; coalesce rapid mod sequences.                         |
| `POST /admin/reload` (API-key gated)           | off     | For platforms that can't deliver signals to the container (some PaaS).      |

The watcher uses `fsnotify.NewWatcher()` plus a debounce ticker. It
must handle ConfigMap atomic-rename updates correctly — Kubernetes
swaps a symlink rather than rewriting the file in place, so we
watch the parent directory and re-read on `Rename` / `Create`.

### 4.4 Listener side: TLS cert rotation

`tls.Config.GetCertificate` is set to a closure that consults
`runtime.Current().Cfg.Server.TLS` on every handshake, calling
`tls.LoadX509KeyPair` only when the cert/key file mtimes have
changed (cached). This is the nginx pattern.

The `http.Server` itself is NOT replaced.

### 4.5 Async pipeline reload

The asynq `Server` is its own goroutine forest. To reload its
config:

1. Stop accepting new tasks: `srv.Shutdown()` (graceful) or
   `srv.Stop()` (immediate).
2. Wait for active tasks to complete (`srv.Shutdown` does this with
   `ShutdownTimeout`).
3. Construct a new `asynq.Server` with the new options.
4. Atomic-swap into the snapshot.

In-flight tasks are picked up by the new worker after restart of the
goroutine — asynq's lease semantics guarantee at-most-once delivery
across this transition. Document the brief gap.

If `tasks.enabled` flips from off to on, we cold-start the Worker
during the swap. Off→on is the "expansion" case; on→off requires
the drain.

---

## 5. Failure modes & guarantees

| Scenario                                                | Behaviour                                                                                              |
|---------------------------------------------------------|--------------------------------------------------------------------------------------------------------|
| New YAML fails `Validate`                              | Reject the reload; log error with `request_id=reload-<ulid>`; running config untouched.                |
| New YAML cannot resolve a required env var             | Reject; same as above.                                                                                  |
| New backend URL DNS fails at startup probe             | Allowed (we don't probe at reload — same as cold start). Reads happen lazily.                          |
| TLS cert rotation: file unreadable                     | `GetCertificate` returns the previous cert; log a warning. Connections continue with the old cert.    |
| Reload changes `server.listen`                         | Reject with explicit message; restart required.                                                         |
| Two reloads race                                        | Serialise via `sync.Mutex` on the reload pipeline; second one queues.                                  |
| Engine retirement: in-flight requests still using it   | Old adapter remains alive until the request finishes; closing its transport triggers
`CloseIdleConnections` only after a configurable grace (default 30 s).                                                                                          |

---

## 6. Routing rebuild — chi gotcha

chi's `Router.Handle` panics on duplicate routes. Today we mount
passthrough prefixes once at startup. Reload that changes
`passthrough.<n>_prefix` would need a new router; live-swapping the
router itself is straightforward (the http.Server holds a single
`Handler`):

```go
type Handler struct {
    inner atomic.Pointer[chi.Mux]
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    h.inner.Load().ServeHTTP(w, r)
}
```

The `http.Server`'s `Handler` is set once to a `*Handler{}`. Reload
rebuilds the `chi.Mux` and atomic-swaps it. Old in-flight requests
finish on the old mux; new requests dispatch on the new one.

This generalises: we won't try to mutate routes in place; we always
rebuild the mux.

---

## 7. Observability of the reload itself

Emit one log entry per reload attempt:

```json
{
  "event": "reload_completed",
  "reload_id": "01HM…",
  "trigger": "sighup|fsnotify|admin",
  "result": "applied|rejected",
  "duration_ms": 14,
  "changed": {
    "routing.default_cee_engine": ["docling", "tika"],
    "engines.tika.url": ["http://tika:9998", "http://tika.svc:9998"]
  },
  "errors": []
}
```

Add Prometheus metrics:

| Metric                                          | Type    | Labels                          |
|-------------------------------------------------|---------|---------------------------------|
| `owui_cee_proxy_reloads_total`                  | counter | `trigger,result`                |
| `owui_cee_proxy_reload_duration_seconds`        | histogram | `trigger`                     |
| `owui_cee_proxy_active_config_generation`       | gauge   | (raw monotonic counter)         |

---

## 8. Implementation phases

### Phase 1 — atomic snapshot foundation (no behaviour change)

1. Introduce `internal/runtime` with `Snapshot` and the atomic
   pointer.
2. Move `*config.Config` references in middleware/handlers to read
   from `runtime.Current()`.
3. Composition root populates the initial snapshot; nothing reloads
   yet.
4. Tests pass unchanged.

**Outcome**: all readers go through the snapshot; reload becomes a
single-pointer write.

### Phase 2 — `SIGHUP` reload of safe scalars

1. Wire `signal.Notify(SIGHUP)`.
2. On signal: re-load YAML, validate, build a NEW snapshot
   reusing engines/clients (no rebuild yet).
3. Apply scalar changes only: `default_cee_engine`,
   `mime_types`, `proxy_api_keys`, `require_api_key`, `log.level`,
   `ratelimit_global`.
4. Reject changes to anything else with a clear error.

**Outcome**: 70% of the operator value (engine selection swap) lands
without any HTTP-stack rebuild.

### Phase 3 — engine fingerprint + transport rebuild

1. Compute fingerprint at registry build time.
2. Reload diff: keep matching, replace mismatched, retire removed.
3. Retirement: track in-flight via a per-adapter `sync.WaitGroup`;
   release transport after `WaitGroup.Wait()` or grace timer fires
   (whichever comes first), then `CloseIdleConnections`.

**Outcome**: changing `engines.tika.url` or `breaker.*` reloads
without restart.

### Phase 4 — router rebuild

1. Refactor `http.Server.Handler` to be a `*runtime.Handler` with
   atomic mux.
2. On reload, rebuild the mux from the new snapshot and swap.
3. Test passthrough route changes.

**Outcome**: `passthrough.*_prefix` is hot-reloadable.

### Phase 5 — TLS cert rotation

1. `tls.Config.GetCertificate` callback consults a shared cert
   cache.
2. Cert cache stat()s the cert file on every handshake; reloads on
   mtime change.
3. SIGHUP also forces an immediate stat.

**Outcome**: Let's-Encrypt-style cert rotation works in-place.

### Phase 6 — async stack reload

1. Wrap `tasks.Worker` with a `sync.Mutex` + restart helper.
2. On reload that touches `tasks.*`: `Shutdown(grace)`, build new,
   atomic-swap.
3. `Orchestrator` is similar (Redis client may be reused if URL
   unchanged).

**Outcome**: `tasks.queue_concurrency` etc. are hot-reloadable.

### Phase 7 — fsnotify watcher

1. Watch the parent directory of the config file; coalesce
   `Rename` + `Create` into a single reload event.
2. Debounce 1 s.
3. Optional: `OWUI_PROXY_RELOAD_ON_FILE_CHANGE` env to disable for
   environments that prefer signal-only.

**Outcome**: Kubernetes ConfigMap edits trigger reload automatically.

### Phase 8 — admin endpoint

1. `POST /admin/reload` gated by `security.admin_api_key_env`.
2. Returns the same JSON as the `reload_completed` log line.

**Outcome**: PaaS without signal delivery can still reload.

### Phase 9 — operational polish

- Reload-reject metric pages and graphs.
- Documentation: "How to reload safely", expectations re: in-flight
  requests, breaker/limiter resets.
- `make reload` Makefile target for local dev (`pkill -HUP -f
  owui-cee-proxy`).

---

## 9. Testing strategy

### Unit
- `runtime.Snapshot` swap is wait-free under concurrent readers.
- Fingerprint equality matches expected engine reuse.
- Validate-reject does not modify current snapshot.

### Integration (testcontainers)
- Start with config A. Send 50 RPS through the proxy. Reload to
  config B mid-stream. Assert: zero 5xx, at most one `request_id`
  observed under each config.
- Reload while a sync request is mid-flight on engine X, after
  engine X has been removed. Assert: in-flight request completes;
  next new request uses the new default.
- Reload while an async task is queued. Assert: poll/result still
  resolve.

### Chaos
- Reload-storm: 100 reloads/sec for 30 s. No leaks (heap stable),
  no goroutine drift (`runtime.NumGoroutine()`).

### Load
- Reload during sustained 200 RPS soak. p99 latency unchanged
  before/after; no >5 s gap in the request rate.

---

## 10. Risks & mitigations

| Risk                                                | Mitigation                                                                |
|-----------------------------------------------------|----------------------------------------------------------------------------|
| Reader / writer race on the snapshot pointer        | `atomic.Pointer[Snapshot]` is the only access path; no shared mutables.    |
| Goroutine leak on engine retirement                 | `sync.WaitGroup` per adapter; retirement waits for it (with grace timer). |
| Breaker reset on engine reuse confuses operators    | Reuse only when fingerprint matches; document clearly.                     |
| Cert reload picks up half-written file              | Atomic mtime check + `tls.LoadX509KeyPair` validates the pair.             |
| Reload deadlocks with shutdown                      | Reload acquires its own mutex; shutdown holds a separate one and signals reload to abort. |
| Bad reload silently truncates running engines       | Validate + dry-run "build snapshot" BEFORE the swap; keep `_, err := buildSnapshot(...)` semantically separate from `swap`. |

---

## 11. Out-of-band: what we explicitly defer

- Process-level zero-downtime restarts via fd handoff. Use a
  rolling restart at the orchestration layer (Kubernetes
  `Deployment`, systemd `Restart=on-failure` + `KillMode=mixed`)
  instead.
- HTTP/3 / QUIC support implies a different listener model; revisit
  if/when needed.
- Live reload of asynq's underlying Redis URL with an open
  connection. Treat this as a process restart for now.

---

## 12. Acceptance criteria (definition of done for the milestone)

A reload pipeline is "shippable" when all of the following hold:

1. ✅ A `kill -HUP $(pidof owui-cee-proxy)` applies all changes from
   §3 marked "✅" within 1 s.
2. ✅ Concurrent 200-RPS soak shows zero non-2xx caused by the
   reload itself.
3. ✅ Heap and goroutine count stay flat (±5 %) across 100 reloads
   in 30 s.
4. ✅ Bad YAML rejects with a structured log line and the running
   config is provably untouched.
5. ✅ TLS cert rotation works without dropping in-flight TLS
   connections.
6. ✅ `POST /admin/reload` works when API-key auth is enabled.
7. ✅ Documentation update in `CLAUDE.md` ("Hot reload contract")
   and `README.md` ("Reload" section).
