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
2. **Speak both OpenWebUI loader protocols.** Inbound: Docling Serve
   (`/v1/convert/file`) **and** OpenWebUI's external loader
   (`PUT /process`). Outbound: any backend whose wire format maps
   to one of the supported `compat_type`s.
3. **Route per-MIME** within a facade — engines declare which MIME
   types they accept; the registry picks the first match.
4. **Stay stateless** — any replica must be able to serve any sync or
   async request.
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
                  ┌────────────────────────────────────────────────────┐
   OpenWebUI ──▶  │                  owui-cee-proxy                    │
                  │                                                    │
                  │   chi.Router                                       │
                  │     ├─▶ docling facade  POST /v1/convert/{file,…}  │
                  │     │                       │                      │
                  │     │                       ▼                      │
                  │     │            Registry.Pick(FacadeDocling,mime) │
                  │     │                                              │
                  │     ├─▶ external facade PUT /process               │
                  │     │                       │                      │
                  │     │                       ▼                      │
                  │     │           Registry.Pick(FacadeExternal,mime) │
                  │     │                                              │
                  │     │                       ▼                      │
                  │     │              Engine.Convert(ctx)             │
                  │     │                                              │
                  │     ├─▶ /<engine-name>/* (passthrough, per engine) │
                  │     │                                              │
                  │     ├─▶ /healthz, /readyz                          │
                  │     ├─▶ /metrics                                   │
                  │     └─▶ /version                                   │
                  │                                                    │
                  │   Optional async path:                             │
                  │     POST  /v1/convert/file/async ─▶ asynq.Enqueue  │
                  │     GET   /v1/status/poll/{id}   ─▶ Inspector      │
                  │     GET   /v1/result/{id}        ─▶ Redis blob     │
                  └────────────────┬─────────────────┬─────────────────┘
                                   │                 │
                       ┌───────────┼─────────────────┼──────────────┐
                       │           │                 │              │
                       ▼           ▼                 ▼              ▼
                 compat:docling  compat:tika    compat:external  compat:docling-external
                 e.g. Docling   Apache Tika    e.g. OpenWebUI    (composite — both
                 Serve, also    /tika/text     external loader,  endpoints on the
                 Kreuzberg      raw PUT        MinerU, custom    same backend)
                 (`/v1/convert/                                  services
                  file`)                                          ┌──────────┐
                                                                  │  Redis   │
                                                                  │ (only when
                                                                  │  tasks.enabled)
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

### 3.1 Docling facade — `POST /v1/convert/file`

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
   `crypto/subtle.ConstantTimeCompare` over a SHA-256 digest.
9. **`Convert.handle`** parses the multipart body. Each part is
   spooled via `handlers/spool.go` — small parts stay in memory,
   parts above an 8 MiB threshold spill to `O_TMPFILE` so the heap
   ceiling stays bounded.
10. **`Registry.Pick(FacadeDocling, mime)`** filters engines whose
    `Capabilities().Facades` includes `FacadeDocling`, then matches
    `mime_types` (exact, then `type/*` wildcard). The default engine
    is the catch-all (must itself support the facade).
11. **`engine.Convert(ctx, req)`** dispatches to the chosen adapter,
    which:
    - opens an `io.Pipe`,
    - spawns a goroutine that writes a multipart body into the pipe
      writer (file part → streaming `io.Copy`, then any forward
      options as form fields),
    - issues the HTTP request to the backend with the pipe as body —
      so the body is **streamed** to the backend.
12. The Docling adapter additionally performs `maybeNormalize` on the
    response body to rewrite `"<field>": null` → `"": ""` for the
    five Docling content fields (defensive against OpenWebUI 0.9.2's
    Pydantic strictness).
13. The handler streams the response body back to the caller via
    `io.Copy`.

### 3.2 Docling facade — `POST /v1/convert/source`

JSON-bodied. URLs are validated against `defaultIsExternalURL`
(rejects loopback, RFC1918, link-local, IPv6 ULA/link-local). On
pass, the request always goes to the **default** engine (no MIME
hint at dispatch time). The handler rejects 400 if the picked
engine's `Capabilities().HTTPSources` is `false`.

### 3.3 External facade — `PUT /process`

OpenWebUI's external loader contract:

- Inbound: raw bytes in body, `Content-Type` is the file's MIME,
  `X-Filename` carries the URL-encoded filename, `Authorization:
  Bearer <token>` (validated via the same APIKey middleware as
  docling facade).
- Lifecycle: same middleware chain as 3.1, but `Convert.Process`
  reads the raw body into a single `FileBlob` via the spool helper,
  builds `ConvertRequest{Facade: FacadeExternal}`, and dispatches via
  `Registry.Pick(FacadeExternal, contentType)`.
- Outbound: depends on the picked engine's `compat_type`:
  - `external`/`docling-external` → passthrough PUT raw to the
    engine's `/process` endpoint.
  - `docling` → adapt to multipart POST `/v1/convert/file`; response
    `document.md_content` is rewrapped as `{page_content, metadata}`.
  - `tika` → adapt to PUT `/tika/text`; response is shaped to the
    external response envelope.
- Response shape: `{page_content, metadata}` (LangChain Document) or
  a list thereof.

### 3.4 Native passthrough — `/<engine-name>/*`

A `*httputil.ReverseProxy` per **enabled** engine, mounted at the
engine's YAML key. Hop-by-hop headers and the inbound
`Authorization` header are stripped; the engine's API key is
injected on the way out using the engine's declared `auth_header` +
`auth_scheme`. Path is `path.Clean`-then-`TrimPrefix`'d and
`singleJoin`'d onto the backend base URL. Body and response are
streamed.

Engines named `main-docling`, `legacy-tika`, `mineru-cloud`, etc.
become `/main-docling/*`, `/legacy-tika/*`, `/mineru-cloud/*`. The
engine name regex (`^[a-z0-9][a-z0-9-]{0,31}$`) ensures path safety.

### 3.5 Async — `POST /v1/convert/file/async` + poll + result

1. The handler parses the multipart body the same way as sync (with
   spooling).
2. **Capability check** — if the picked engine reports
   `Capabilities().HTTPSources == false` and the request is in
   source mode, respond 400 immediately (no enqueue). This replaces
   the v0.0.x hard-coded `eng.Name() != engine.Docling` check.
3. `tasks.PayloadFromRequest` produces a `Payload` carrying file
   contents + `Facade` (currently always `FacadeDocling` for the
   async path).
4. `Orchestrator.Enqueue`:
   - For each file, generates a UUID-keyed Redis key, `SET`s the
     bytes with TTL = `tasks.retention`, and stores
     `BlobRef{key,filename,content_type,size}` in the payload.
   - Returns an opaque token bound to the requesting API key (B3 fix).
   - `client.EnqueueContext(t, MaxRetry, Retention, Timeout)` hands
     the JSON-encoded payload to asynq.
5. Any replica's `Worker` (asynq server) eventually picks the task:
   - For each `BlobRef`, `LoadBlob` from Redis, build a
     `bytes.Reader`-backed `engine.FileBlob`.
   - Build a `ConvertRequest` with `Facade` from the payload.
   - Call the adapter's `Convert` exactly as the sync path would.
   - `SaveResult` persists the response body + content-type under
     `owui-cee:result:<task_id>` with TTL = `tasks.result_ttl`.
6. `Async.Poll` queries asynq's `Inspector` for the task state.
7. `Async.Result` reads the result blob from Redis (token-bound) and
   streams it back.

Because all task state lives in Redis, **any replica can serve the
poll/result of any task**. Sticky sessions are not required.

---

## 4. Engine adapter contract

```go
type Facade string

const (
    FacadeDocling  Facade = "docling"
    FacadeExternal Facade = "external"
)

type EngineCapabilities struct {
    Facades     []Facade // which inbound facades this engine can serve
    HTTPSources bool     // supports /v1/convert/source
}

type ConvertRequest struct {
    Files     []FileBlob
    Sources   []HTTPSource
    Options   map[string]string
    Headers   http.Header
    RequestID string
    Facade    Facade // which facade the client used
}

type Engine interface {
    Name() Name
    Capabilities() EngineCapabilities
    Convert(ctx context.Context, req *ConvertRequest) (*ConvertResponse, error)
    Health(ctx context.Context) error
}

type RegistryEntry struct {
    Engine    Engine
    MimeTypes []string
}
```

### compat_type matrix (v0.2.0)

| compat_type        | Backend speaks                                     | Capabilities.Facades       | HTTPSources |
|--------------------|----------------------------------------------------|----------------------------|-------------|
| `docling`          | Docling Serve `/v1/convert/{file,source}`          | `[FacadeDocling]`          | `true`      |
| `external`         | OpenWebUI `PUT /process` raw, `page_content`       | `[FacadeExternal]`         | `false`     |
| `docling-external` | both endpoints on the same backend (separate paths)| `[FacadeDocling, External]`| `true`      |
| `tika`             | Tika `PUT /tika/text` raw, proprietary JSON        | `[FacadeDocling, External]`| `false`     |

Tika's adapter shapes its output for both inbound facades (it
re-wraps the proprietary Tika JSON into either the docling envelope
or the external `{page_content,metadata}` envelope). Kreuzberg is
**not** a separate compat — it's a `compat_type: docling` engine.

The shared contract is enforced at compile time by `engine.Engine`
and at test time by `internal/engine/enginetest.RunContractTests`,
which now also asserts `Capabilities().Facades` is non-empty.

### Adding a new compat_type

To add e.g. `marker` (a Marker-specific PDF→Markdown service that
neither speaks Docling nor external):

1. Create `internal/engine/compat/marker/{marker.go, transform.go, marker_test.go}`.
2. Add `CompatMarker = "marker"` to the constant block in
   `internal/config/config.go` and extend the `compat_type` enum
   validator.
3. Wire it into `internal/app/app.go::newAdapterByCompat` (one switch case).
4. Document the new compat in this file's matrix.

No handlers, middleware, async pipeline, or routes change.

---

## 5. MIME-based routing

`Registry.Pick(facade, mime)` is invoked exactly once per
docling-facade or external-facade request, with the first uploaded
file's `Content-Type` (params stripped, lower-cased).

Pattern syntax:

- `application/pdf` — exact
- `image/*` — top-level wildcard
- `*/*` — universal; honour with care

The pick is **filtered by facade first**: only engines whose
`Capabilities().Facades` includes the inbound facade are
candidates. Within that subset, default-engine `mime_types` is
**deliberately ignored** so the default stays a true catch-all.
Non-default engines declared in alphabetical order are scanned
(registry sorts at startup); first matching pattern wins. If the
default engine itself does not support the facade,
`Registry.PickWithError` returns an error and the handler emits 400.

`/v1/convert/source` skips MIME routing because the proxy never
dereferences URLs (an SSRF mitigation by construction). The default
engine handles every source request.

Caller-controlled MIME is a known consideration: an attacker can
mislabel a `.docx` as `application/pdf` to choose which engine
handles it. The mitigation in scope is per-engine isolation
(separate `*http.Transport`, breaker, rate limit) plus the SSRF
block-list. Magic-byte sniffing is documented as a future
enhancement.

---

## 6. Buffering decision (the multipart trap)

Go's `mime/multipart.Reader.NextPart()` **drains** the previous part
when called. Any code that collects parts into a slice then iterates
without reading each part body sends empty bodies to the engine
adapter. This was a real silent-data-loss bug fixed in
`internal/transport/http/handlers/convert.go`.

The fix is to spool each part fully via `handlers/spool.go` before
calling `NextPart()` again. Spool keeps small parts in memory and
spills large parts to `O_TMPFILE` (Linux) so the heap ceiling stays
bounded regardless of `server.max_body_bytes`.

Outbound to the engine remains streaming (`io.Pipe`-based). Inbound
is bounded but no longer fully heap-allocated.

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
names the env var, the loader reads it. `Validate` blocks startup
on cross-field rule violations:

- `routing.default_engine` must reference an enabled engine.
- For every enabled engine, `compat_type` must be in the enum and
  `url` must be non-empty.
- For each enabled facade, at least one engine must declare that
  facade in its `Capabilities().Facades`.
- Engine names match `^[a-z0-9][a-z0-9-]{0,31}$`.
- `auth_header` matches `^[A-Za-z][A-Za-z0-9-]{0,63}$`.

Override syntax (note: the engine map key is part of the env path):

```sh
OWUI_PROXY_ENGINES__MAIN-DOCLING__URL=http://docling.svc:5001
OWUI_PROXY_ROUTING__DEFAULT_ENGINE=main-docling
OWUI_PROXY_ROUTING__FACADE__EXTERNAL__ENABLED=true
OWUI_PROXY_OBSERVABILITY__LOG__LEVEL=debug
```

A v0.1.x → v0.2.0 migration script is available at
`scripts/migrate-config/`; see `docs/MIGRATION-v0.2.0.md`.

---

## 8. Observability

- **Logs**: structured JSON to stdout via zerolog. Every completed
  request line carries `request_id`, `method`, `path`, `status`,
  `duration`, `engine`, `engine_url`, `filename`, `file_count`,
  `bytes_in`, `bytes_out`. The `engine` + `filename` pair fulfils
  the operational requirement to see at a glance which file went
  where.
- **Metrics**: Prometheus exposition at `/metrics`, RED metrics per
  engine + breaker state + body bytes histograms (in + out) +
  `chi.RouteContext().RoutePattern()`-collapsed `route` label.
- **Tracing**: optional OpenTelemetry HTTP server instrumentation +
  OTLP HTTP exporter when `tracing.enabled=true`.

---

## 9. Security

The full threat model lives in `SECURITY.md`. The architectural
load-bearing pieces:

- **No secrets in YAML.** Only env-var names live in YAML.
- **Per-engine transport isolation.** A misbehaving engine's idle
  pool, breaker state, and rate limiter cannot bleed into another
  engine's lane.
- **Hop-by-hop sanitisation** on every passthrough request. Inbound
  `Authorization` is dropped. The engine's own auth header (declared
  in config) is re-injected.
- **SSRF block-list** on `/v1/convert/source`. Conservative
  deny-by-default for private/loopback/link-local IPv4 and IPv6,
  plus the cloud metadata IPs and a port allowlist.
- **Engine-name + auth-header validation** at config load time so
  malicious YAML can't shadow facade routes or inject CRLF into
  outbound headers.
- **Filename sanitisation** at the docling and external facade
  boundaries — CR/LF/NUL rejected before the value is logged or
  forwarded.
- **SHA-256 API-key compare** (no length leak).
- **Distroless image, non-root, read-only rootfs, dropped caps.**
- **Hardened systemd unit.**

---

## 10. Decision log

| Date       | Decision                                                                              | Why                                                                  |
|------------|---------------------------------------------------------------------------------------|----------------------------------------------------------------------|
| 2026-05-10 | Hybrid API surface: Docling-compat facade + per-engine native passthrough            | OpenWebUI binds to one URL; advanced users still want native APIs.   |
| 2026-05-10 | YAML-only default engine with MIME-based per-request routing on top                  | Zero per-request overhead, no header surface for clients to abuse.   |
| 2026-05-10 | Async via asynq + Redis (external state); workers in-process                          | Stateless replicas, no sticky sessions; one binary to operate.        |
| 2026-05-10 | Buffer multipart parts via spool helper at the handler                                | `multipart.Reader.NextPart()` drains previous parts (silent bug).    |
| 2026-05-10 | Kreuzberg uses `compat_type: docling` (its `/v1/convert/file` is docling-compat)      | Kreuzberg already returns Docling-shape; no separate adapter.        |
| 2026-05-10 | Docling adapter normalises `null` content fields → `""` post-flight                   | OpenWebUI 0.9.2 Pydantic loader rejects `None`.                       |
| 2026-05-10 | Per-engine `auth_header` + `auth_scheme` (raw \| bearer) replaces `apiKeyHeaderFor`   | Generalises auth so engines remain transport-agnostic for the proxy. |
| 2026-05-10 | `Engine.Capabilities()` declares supported facades + http-sources support             | Replaces hard-coded `eng.Name() != engine.Docling` check (M14).      |
| 2026-05-10 | Both docling and external facades exposed by default; engines opt in via Capabilities | OpenWebUI ships two loader env vars; supporting both is one config flip. |
| 2026-05-10 | `compat_type: docling-external` is a composite that holds two child adapters          | Backends serving both endpoints don't need a bespoke wire format.    |
| 2026-05-10 | Engine names are user-controlled and become URL path segments — strict regex          | Path safety + log readability.                                       |
| 2026-05-10 | Tika's adapter shapes output for both facades (docling + external envelopes)          | Tika's wire format is sui generis; one adapter, two outbound shapes. |

---

## 11. Future work (snapshot)

See `docs/REVIEW.md` for the prioritised list. High-impact items
post v0.2.0:

1. Per-facade `/readyz` labels so operators can tell which facade is
   broken on a `docling-external` composite.
2. `marker`, `mineru-direct`, `paddle` compat presets — the
   architecture makes them trivial; add as separate PRs once
   v0.2.0 is in.
3. Magic-byte MIME sniffing as an opt-in fallback when callers don't
   set `Content-Type` honestly.
4. Move metrics `route` label to bind on chi route patterns even for
   passthrough mounts (currently uses raw URL path for those).
5. Async result token rotation (current binding is per-API-key
   indefinitely; rotate on key revocation).
