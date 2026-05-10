# Architecture

A self-contained reference for what this binary actually is, how it
processes a request, and the decisions that look load-bearing today.
Read alongside `CLAUDE.md` (which encodes the invariants in
imperative terms).

---

## 1. Goals

1. **Centralise** OpenWebUI's content-extraction backends behind a
   single endpoint so operators can swap engines without patching
   OpenWebUI.
2. **Route per-MIME** — Docling-supported types go to Docling, the
   rest go to Kreuzberg (or whichever engine claims that MIME).
3. **Stay stateless** — any replica must be able to serve any sync or
   async request.
4. **Be Docling-compatible** by default — OpenWebUI sees the proxy as
   a Docling Serve clone, regardless of which engine handled the
   request.
5. **Native passthrough** for advanced users who want to hit a
   backend's full native API.

Non-goals:

- Persistent extraction caching.
- Authentication systems beyond a shared API key (use an external
  ingress / OIDC sidecar for richer schemes).
- Rendering / OCR features the backend itself does not provide.

---

## 2. Topology

```
                  ┌──────────────────────────────────────────────────┐
   OpenWebUI ──▶  │                  owui-cee-proxy                  │
                  │                                                  │
                  │   chi.Router  ─┬─▶  facade  /v1/convert/{file,   │
                  │                │             source,…}           │
                  │                │              │                  │
                  │                │              ▼                  │
                  │                │       Registry.Pick(mime)       │
                  │                │              │                  │
                  │                │              ▼                  │
                  │                │       Engine.Convert(ctx)       │
                  │                │                                  │
                  │                ├─▶  /docling/* (passthrough)     │
                  │                ├─▶  /tika/*    (passthrough)     │
                  │                ├─▶  /kreuzberg/* (passthrough)   │
                  │                │                                  │
                  │                ├─▶  /healthz, /readyz             │
                  │                ├─▶  /metrics                      │
                  │                └─▶  /version                      │
                  │                                                  │
                  │   Optional async path:                           │
                  │     POST  /v1/convert/file/async ─▶ asynq.Enqueue│
                  │     GET   /v1/status/poll/{id}   ─▶ Inspector    │
                  │     GET   /v1/result/{id}        ─▶ Redis blob   │
                  └────────────────┬───────────────────┬─────────────┘
                                   │                   │
                       ┌───────────┴───────┐   ┌───────┴────────────┐
                       │                   │   │                    │
                       ▼                   ▼   ▼                    ▼
                  Docling Serve      Apache Tika              Kreuzberg
                  /v1/convert/{file, /tika/text PUT raw       /v1/convert/file
                   source}          /version GET              (docling-compat)
                                                              /health
                                                          ┌──────────┐
                                                          │  Redis   │ (only when tasks.enabled)
                                                          └──────────┘
```

The proxy itself holds **no** persistent state. The only mutable
runtime state is in:

- per-backend `*http.Transport` connection pools (in-memory)
- per-backend circuit breakers (in-memory)
- the global token-bucket rate limiter (in-memory)
- async task state (Redis — external)

A pod restart loses the breaker / rate-limiter counters and any
in-flight sync request. Async requests are durable.

---

## 3. Request lifecycle

### 3.1 Sync facade — `POST /v1/convert/file`

1. **`RequestID`** middleware stamps a ULID into the context and the
   `X-Request-ID` response header.
2. **`Recover`** wraps everything below; panics become 500s with a
   stack-trace log line.
3. **`OTel`** opens a server span tagged with method/path/status.
4. **`GlobalRateLimit`** consumes a token (or 429s).
5. **`AccessLog`** allocates a mutable `requestMeta` pointer and
   stuffs it into the context. The handler later mutates it; on
   `next.ServeHTTP` return, the middleware reads `engine`,
   `engine_url`, `filename`, `file_count` and emits one
   `request_completed` JSON line.
6. **`Metrics`** reads the same `requestMeta` for label values.
7. **`BodyLimit`** wraps `r.Body` with `http.MaxBytesReader`.
8. **`APIKey`** (only inside the authenticated subtree) compares
   `X-Proxy-Api-Key` against the configured allow-list with
   `crypto/subtle.ConstantTimeCompare`.
9. **`Convert.handle`** parses the multipart body. **Each part is
   buffered fully into a `*bytes.Buffer`** before the loop advances
   to the next part — see "Buffering decision" below for why.
10. **`Registry.Pick(req.Files[0].ContentType)`** scans every enabled
    non-default engine's `mime_types`, exact match then `type/*`
    wildcard. First match wins. Default engine is the catch-all.
11. **`engine.Convert(ctx, req)`** dispatches to the chosen adapter,
    which:
    - opens an `io.Pipe`,
    - spawns a goroutine that writes a multipart body into the pipe
      writer (file part → `io.Copy(fw, f.Body)`, then any forward
      options as form fields),
    - issues the HTTP request to the backend with the pipe as body —
      so the body is **streamed** to the backend.
12. The Docling adapter additionally performs `maybeNormalize` on the
    response body to rewrite `"<field>": null` → `"": ""` for the
    five Docling content fields (defensive against OpenWebUI 0.9.2's
    Pydantic strictness).
13. The handler streams the response body back to the caller via
    `io.Copy`.

### 3.2 Sync facade — `POST /v1/convert/source`

JSON-bodied. URLs are validated against `defaultIsExternalURL`
(rejects loopback, RFC1918, link-local, IPv6 ULA/link-local). On
pass, the request always goes to the **default** engine (no MIME
hint at dispatch time).

### 3.3 Native passthrough — `/{docling,tika,kreuzberg}/*`

A `*httputil.ReverseProxy` per engine. Hop-by-hop headers and the
inbound `Authorization` header are stripped; the engine's API key is
injected on the way out. Path is `TrimPrefix`'d and `singleJoin`'d
onto the backend base URL. Body and response are streamed.

### 3.4 Async — `POST /v1/convert/file/async` + poll + result

1. The handler parses the multipart body the same way as sync.
2. `tasks.PayloadFromRequest` produces a `Payload` carrying file
   contents in temporary in-memory buffers.
3. `Orchestrator.Enqueue`:
   - For each file, generates a UUID-keyed Redis key, `SET`s the
     bytes with TTL = `tasks.retention`, and stores
     `BlobRef{key,filename,content_type,size}` in the payload.
   - `client.EnqueueContext(t, MaxRetry, Retention, Timeout)` hands
     the JSON-encoded payload to asynq.
4. Any replica's `Worker` (asynq server) eventually picks the task:
   - For each `BlobRef`, `LoadBlob` from Redis, build a
     `bytes.Reader`-backed `engine.FileBlob`.
   - Call the adapter's `Convert` exactly as the sync path would.
   - `SaveResult` persists the response body + content-type under
     `owui-cee:result:<task_id>` with TTL = `tasks.result_ttl`.
5. `Async.Poll` queries asynq's `Inspector` for the task state.
6. `Async.Result` reads the result blob from Redis and streams it
   back.

Because all task state lives in Redis, **any replica can serve the
poll/result of any task**. Sticky sessions are not required.

---

## 4. Engine adapter contract

```go
type Engine interface {
    Name() Name
    Convert(ctx context.Context, req *ConvertRequest) (*ConvertResponse, error)
    Health(ctx context.Context) error
}

type RegistryEntry struct {
    Engine    Engine
    MimeTypes []string
}
```

Three adapters today:

| Adapter   | Endpoint hit                                                  | Response shaping                                       |
|-----------|----------------------------------------------------------------|---------------------------------------------------------|
| Docling   | `POST /v1/convert/{file,source}` (native facade)              | `maybeNormalize` rewrites `null` content fields → `""`  |
| Tika      | `PUT /tika/text` per file, JSON aggregated client-side        | `parseTikaResponse` + `wrapDoclingResponse` envelope    |
| Kreuzberg | `POST /v1/convert/file` (Kreuzberg's "openweb" docling-compat) | None — Kreuzberg already returns `document.md_content`  |

Adding a fourth engine touches only:

1. `internal/engine/<name>/{<name>.go,transform.go (optional),<name>_test.go}`
2. `internal/config/config.go` — add a `<Name>` field on `EnginesConfig`
3. `internal/app/app.go` `buildRegistry` — one branch
4. `configs/config.example.yaml`, `deployments/helm/owui-cee-proxy/values.yaml`
5. `internal/transport/http/routes.go` `apiKeyHeaderFor` — only if
   the engine uses something other than `X-Api-Key`

Routes, middleware, handlers, async pipeline, observability — all
unchanged.

The shared contract is enforced at compile time by `engine.Engine`
and at test time by `internal/engine/enginetest.RunContractTests`.

---

## 5. MIME-based routing

`Registry.Pick(mime)` is invoked exactly once per request, with the
first uploaded file's `Content-Type` (params stripped, lower-cased).

Pattern syntax:

- `application/pdf` — exact
- `image/*` — top-level wildcard
- `*/*` — universal; honour with care

Default-engine `mime_types` is **deliberately ignored** so the
default stays a true catch-all. Non-default engines declared in
alphabetical order are scanned (registry sorts at startup); first
matching pattern wins.

`/v1/convert/source` skips MIME routing because the proxy never
dereferences URLs (an SSRF-mitigation by construction). The default
engine handles every source request.

Caller-controlled MIME is a known consideration: an attacker can
mislabel a `.docx` as `application/pdf` to choose which engine
handles it. The mitigation in scope is per-engine isolation
(separate `*http.Transport`, breaker, rate limit) plus the SSRF
block-list. Magic-byte sniffing is documented as a future
enhancement (see `docs/REVIEW.md`).

---

## 6. Buffering decision (the multipart trap)

Go's `mime/multipart.Reader.NextPart()` **drains** the previous part
when called. Any code that collects parts into a slice then iterates
without reading each part body sends empty bodies to the engine
adapter. This was a real silent-data-loss bug fixed in
`internal/transport/http/handlers/convert.go`.

The fix was to read each part fully into a `*bytes.Buffer` before
calling `NextPart()` again. The cap is the global
`server.max_body_bytes` (default 500 MiB).

The trade-off: outbound to the engine remains streaming
(`io.Pipe`-based), but the inbound body sits in memory at the
handler boundary. This is an explicit exception to the
"streaming-end-to-end" aspiration documented in `CLAUDE.md`. A
follow-up to spool large bodies to a temp file (`O_TMPFILE` on
Linux) is on the roadmap (see `docs/REVIEW.md` HIGH #1).

---

## 7. Configuration

```
defaults  ──merge──▶  YAML  ──merge──▶  env (OWUI_PROXY_*)
   │                    │                    │
   └─ config.Default()  └─ koanf-yaml        └─ koanf-env
                                                  │
                                                  └─ resolveSecrets
                                                        │
                                                        └─ os.Getenv per
                                                          api_key_env /
                                                          redis_url_env /
                                                          proxy_api_keys_env
```

Three layers, last write wins. Secrets never sit in YAML; the YAML
names the env var, the loader reads it. The `Validate` step blocks
startup on cross-field rule violations (default engine must be
enabled, async needs `REDIS_URL`, TLS needs cert+key).

Override syntax:

```sh
OWUI_PROXY_ENGINES__TIKA__URL=http://tika.svc:9998
OWUI_PROXY_ROUTING__DEFAULT_CEE_ENGINE=tika
OWUI_PROXY_OBSERVABILITY__LOG__LEVEL=debug
```

---

## 8. Observability

- **Logs**: structured JSON to stdout via zerolog. Every completed
  request line carries `request_id`, `method`, `path`, `status`,
  `duration`, `engine`, `engine_url`, `filename`, `file_count`,
  `bytes_out`. The `engine` + `filename` pair fulfils the operational
  requirement to see at a glance which file went where.
- **Metrics**: Prometheus exposition at `/metrics`, RED metrics per
  engine + breaker state + body bytes histogram. Series listed in
  README.md.
- **Tracing**: optional OpenTelemetry HTTP server instrumentation +
  OTLP HTTP exporter when `tracing.enabled=true`.

---

## 9. Security

The full threat model lives in `SECURITY.md`. The architectural
load-bearing pieces:

- **No secrets in YAML.** Only env-var names live in YAML.
- **Per-backend transport isolation.** A misbehaving backend's idle
  pool, breaker state, and rate limiter cannot bleed into another
  engine's lane.
- **Hop-by-hop sanitisation** on every passthrough request. Inbound
  `Authorization` is dropped. The engine's own API key is
  re-injected.
- **SSRF block-list** on `/v1/convert/source`. The block-list is
  conservative (deny-by-default for private/loopback/link-local IPv4
  and IPv6, plus the cloud metadata IP). DNS-resolves before fetch;
  see `docs/REVIEW.md` for the known TOCTOU caveat.
- **Constant-time API-key compare** (with the length-leak caveat
  documented in REVIEW.md).
- **Distroless image, non-root, read-only rootfs, dropped caps.**
- **Hardened systemd unit.**

---

## 10. Decision log

| Date       | Decision                                                                              | Why                                                                  |
|------------|---------------------------------------------------------------------------------------|----------------------------------------------------------------------|
| 2026-05-10 | Hybrid API surface: Docling-compat facade + per-engine native passthrough            | OpenWebUI binds to one URL; advanced users still want native APIs.   |
| 2026-05-10 | YAML-only default engine with MIME-based per-request routing on top                  | Zero per-request overhead, no header surface for clients to abuse.   |
| 2026-05-10 | Async via asynq + Redis (external state); workers in-process                          | Stateless replicas, no sticky sessions; one binary to operate.        |
| 2026-05-10 | Buffer multipart parts into memory at the handler                                     | `multipart.Reader.NextPart()` drains previous parts (silent bug).    |
| 2026-05-10 | Kreuzberg adapter forwards to `/v1/convert/file` (its own docling-compat endpoint)    | Kreuzberg already returns Docling-shape; transformation was wasted.  |
| 2026-05-10 | Docling adapter normalises `null` content fields → `""` post-flight                   | OpenWebUI 0.9.2 Pydantic loader rejects `None`.                       |
| 2026-05-10 | Engine API key header is per-engine: `X-Api-Key` for Docling/Tika, `Authorization: Bearer` for Kreuzberg | Each upstream uses what it documents.                  |
| 2026-05-10 | `routes.apiKeyHeaderFor` is the only "engine knows about transport" hook needed       | Otherwise transport remains engine-agnostic.                          |
| 2026-05-10 | `request_completed` access log is the contract for "which file went where"            | Operator UX requirement; metrics carry the same labels.              |

---

## 11. Future work (snapshot)

See `docs/REVIEW.md` for the prioritised list and `docs/HOT_RELOAD_ROADMAP.md`
for the in-flight reload plan. High-impact items:

1. Stream-spool large multipart parts to a temp file instead of
   buffering.
2. Bind async result tokens to the requesting API key so callers
   cannot fetch each other's results.
3. Move metrics `route` label to `chi.RouteContext().RoutePattern()`
   to bound cardinality.
4. Wire the `Timeout` middleware into `routes.go`.
5. Honour `security.trusted_proxies` when stamping
   `X-Forwarded-*` headers in the passthrough mounts.
6. Hot reload of YAML without a process restart (separate doc).
