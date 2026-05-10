# Project review — known issues, risks, and follow-ups

A consolidated audit of the owui-cee-proxy codebase as of 2026-05-10.
Two specialist passes were performed (security/correctness and
performance/maintainability). Findings are merged below in a single
prioritised list. Each item includes severity, location, the issue
in one sentence, the operational impact, and a concrete fix.

Severity guide:

- **blocker** — would gate a 1.0 release; security or correctness
  defect with realistic exploit / data-loss path
- **high** — should be fixed before broad production rollout
- **medium** — fix during normal hardening work
- **low** — improves robustness; no urgency
- **nit** — cosmetic, future-tidy

---

## Blocker

### B1 · SSRF DNS-rebinding TOCTOU on `/v1/convert/source`

- **Where**: `internal/transport/http/handlers/ssrf.go`
  (`defaultIsExternalURL`)
- **Issue**: The validator calls `net.LookupIP` once. The actual
  fetch later goes through `http.Client.Do`, which resolves the
  hostname again. A DNS server returning a public IP for the first
  resolution and `169.254.169.254` for the second bypasses the block-list.
- **Impact**: Attacker can read cloud-instance metadata or hit
  internal services from the proxy's network position.
- **Fix**: Resolve once, then perform the fetch with a custom
  `*net.Dialer.Control` hook (or rewrite the URL to use the
  validated literal IP and set the original hostname in `Host:`).

### B2 · SSRF block-list is incomplete

- **Where**: `internal/transport/http/handlers/ssrf.go` `blockedCIDRs`
- **Issue**: Missing `0.0.0.0/8`, `100.64.0.0/10` (CGNAT),
  `192.0.0.0/24`, `198.18.0.0/15`, AWS/GCP IPv6 metadata
  (`fd00:ec2::254`), IPv4-mapped IPv6, and there is no port allowlist
  (Redis, SSH, etc., on a public host are reachable).
- **Impact**: SSRF mitigation is partial.
- **Fix**: Extend CIDR list, additionally call
  `ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsMulticast()`,
  and restrict ports to `{80, 443}`.

### B3 · Async result is fetchable by anyone who learns the task ID

- **Where**: `internal/tasks/orchestrator.go` (`Result`),
  `internal/transport/http/handlers/async.go` (`Async.Result`)
- **Issue**: `task_id` returned by `EnqueueContext` is the
  authentication mechanism. A second caller who guesses or scrapes a
  task ID gets the result body — there is no caller binding.
- **Impact**: In a multi-tenant deployment, one tenant can read
  another tenant's extracted documents.
- **Fix**: Mint a server-side opaque token at enqueue and store
  `token → task_id` separately; bind the token to the API key (or
  the calling identity from a future auth integration); validate on
  poll/result. Rate-limit result lookups.

---

## High

### H1 · Multipart parts are buffered to memory, capped only by the global body size

- **Where**: `internal/transport/http/handlers/convert.go`
  (`buildFileRequest`); same pattern in `internal/tasks/payload.go`
- **Issue**: `io.ReadAll(part)` allocates one contiguous slab per
  uploaded file. With `max_body_bytes` defaulted to 500 MiB, a
  single request takes 500 MiB of heap.
- **Impact**: Memory blowup / OOM under modest concurrency; a worker
  pod with 512 MiB limit will OOM at one large request.
- **Fix**: Spool parts above a threshold (e.g. 8 MiB) to a temp file
  using `O_TMPFILE` on Linux; pass the file as `io.Reader`. The
  outbound multipart writer goroutine already streams, so the
  end-to-end memory ceiling drops to the threshold.

### H2 · Async path duplicates blob in memory then in Redis with no per-blob cap

- **Where**: `internal/tasks/payload.go` (`payloadFromMultipart`),
  `internal/tasks/orchestrator.go` (`Enqueue`)
- **Issue**: The full file body is buffered in `*bytes.Buffer`, then
  `SET` to Redis. No `max_blob_bytes` config, no streaming.
- **Impact**: Redis can be filled by a small number of large
  uploads. Combined with H1, a single request occupies 500 MiB of
  heap and 500 MiB of Redis.
- **Fix**: Add `tasks.max_blob_bytes`. Reject above. Stream the blob
  upload using `redis.Pipeline` with chunked `SETRANGE` (or move to
  S3-compatible blob storage and store only the URL in the task
  payload).

### H3 · Reverse-proxy never honours `security.trusted_proxies`

- **Where**: `internal/transport/http/reverseproxy/proxy.go` (line
  ~45 — the `pr.SetXForwarded()` call)
- **Issue**: `TrustedProxies` is in the config struct but never
  consulted. `httputil.ProxyRequest.SetXForwarded()` (Go 1.20+)
  appends to incoming `X-Forwarded-For`, so backends see attacker-controlled
  values when the proxy is internet-facing.
- **Impact**: Spoofed client IP / host headers reach the backend.
- **Fix**: Strip inbound `X-Forwarded-*` and `Forwarded` before
  `SetXForwarded`, except when `r.RemoteAddr`'s IP is in
  `TrustedProxies`. Default-deny posture.

### H4 · Async response Content-Type is replayed verbatim from upstream

- **Where**: `internal/transport/http/handlers/async.go`
  (`Async.Result`)
- **Issue**: Whatever Tika/Docling emitted is copied to the response.
  When the proxy is browser-reachable (admin UI, CDN, etc.), this is
  a stored-XSS vector if a backend echoes attacker content.
- **Impact**: XSS / arbitrary client-side code execution against
  proxy origin.
- **Fix**: Set `X-Content-Type-Options: nosniff` on every response;
  constrain `Async.Result` content-type to a known list (`application/json`,
  `text/markdown`, `text/plain`).

### H5 · Filename header injection in outbound multipart

- **Where**: `internal/engine/docling/docling.go` (`createFilePart`),
  `internal/engine/kreuzberg/kreuzberg.go` (`createFilePart`)
- **Issue**: `escapeQuotes` only handles `\` and `"`. Filenames from
  the inbound multipart part are formatted into
  `Content-Disposition: form-data; name="files"; filename="<…>"`.
  A filename containing `\r\n` injects a fake header into the
  outbound multipart body.
- **Impact**: Backend may parse a smuggled second part or get
  confused; depends on backend strictness.
- **Fix**: Reject filenames with control bytes (CR/LF/NUL/etc.) at
  the handler boundary, OR encode filename per RFC 5987 (`filename*=UTF-8''…`).

### H6 · Prometheus `route` label uses raw URL path

- **Where**: `internal/transport/http/middleware/metrics.go`
- **Issue**: `r.URL.Path` is used as the label, so async polling
  paths like `/v1/status/poll/<uuid>` create unbounded label
  cardinality.
- **Impact**: Prometheus storage / cardinality explosion; 404 fuzzers
  amplify the problem.
- **Fix**: `chi.RouteContext(r.Context()).RoutePattern()` to
  collapse `/v1/status/poll/{id}` back to its route pattern.

### H7 · Async task execution timeout = retention (24 h default)

- **Where**: `internal/tasks/orchestrator.go` (`asynq.Timeout(o.cfg.Retention)`)
- **Issue**: The asynq `Timeout` option (per-execution deadline) is
  set to the retention window. A stuck worker holds a Redis lease
  for 24 h before retry.
- **Impact**: Stuck tasks pin Redis state and worker slots far
  beyond what's reasonable.
- **Fix**: Add `tasks.task_timeout` (e.g. 5 min default), use it for
  `asynq.Timeout`. Keep `Retention` for state lifetime only.

### H8 · `mTLS = require` accepts any client cert

- **Where**: `internal/transport/http/server.go` (`buildTLS`)
- **Issue**: `tls.RequireAnyClientCert` does not validate against
  `ClientCAs`. Operators choosing `require` likely expect verification.
- **Impact**: False sense of mTLS security.
- **Fix**: Either drop `require` from the enum (advertise only
  `require-and-verify`), or document explicitly.

### H9 · Caller-controlled `X-Request-ID` is propagated to backends

- **Where**: `internal/engine/docling/docling.go` (`applyForwardHeaders`)
- **Issue**: A client-supplied `X-Request-ID` overrides the
  server-generated ULID.
- **Impact**: Log/correlation poisoning; trace-collision attacks.
- **Fix**: Always overwrite with the server ULID (validate format if
  preserving inbound, e.g. `^[0-9A-HJKMNP-TV-Z]{26}$`).

---

## Medium

### M1 · `Timeout` middleware is implemented but never wired

- **Where**: `internal/transport/http/middleware/timeout.go`,
  `internal/transport/http/routes.go`
- **Issue**: Per-request deadlines depend solely on engine adapters'
  `WithDeadline`. A misbehaving handler holds connections forever.
- **Fix**: Add `r.Use(mw.Timeout(d.Config.Server.WriteTimeout))` (or
  a dedicated `request_timeout`) inside the authenticated subtree.

### M2 · `WriteTimeout` defaults to 0 (disabled)

- **Where**: `internal/config/config.go` (`Default.Server.WriteTimeout`)
- **Issue**: Slow-loris on the response path.
- **Fix**: Default to `5m` and document; large extractions can lift
  it.

### M3 · Reverse-proxy path traversal — `path.Clean` not applied

- **Where**: `internal/transport/http/reverseproxy/proxy.go`
  (`singleJoin`)
- **Issue**: `/docling/../admin` produces `out.URL.Path = u.Path + "/../admin"`.
  Net/url does not collapse this; backends that don't normalise see
  the traversal.
- **Fix**: `out.URL.Path = path.Clean(singleJoin(...))`; reject
  `..` segments after clean.

### M4 · Health endpoint echoes upstream error strings

- **Where**: `internal/transport/http/handlers/health.go`
  (`Readiness`)
- **Issue**: Internal hostnames / TLS error chains reach `/readyz`
  responders.
- **Fix**: Generic `"unhealthy"` in body, full error in stdout
  log only.

### M5 · Convert handler reflects engine error verbatim

- **Where**: `internal/transport/http/handlers/convert.go`
  (`handle`)
- **Issue**: `engine X: dial tcp 10.0.0.5:8080` reaches the caller.
- **Fix**: Log full error with `request_id`; respond with redacted
  message + `request_id`.

### M6 · Status lookup scans every queue

- **Where**: `internal/tasks/orchestrator.go` (`Status`)
- **Issue**: One Redis round-trip per asynq queue per `/poll`.
- **Fix**: Persist `task_id → queue` index at enqueue, or use
  `Inspector.GetTaskInfo` once.

### M7 · `ResponseHeaderTimeout` (30 s) caps long extractions

- **Where**: `internal/httpclient/client.go`
- **Issue**: Tika/Docling on large PDFs commonly take >30 s before
  emitting the first response header. The error path returns 502
  long before `RequestTimeout` (120 s) trips.
- **Fix**: Default to match `RequestTimeout`, or document the
  interaction; raise per-engine.

### M8 · `responseWriter` lacks `Hijacker` / atomic `wrote`

- **Where**: `internal/transport/http/middleware/logging.go`
- **Issue**: Future websocket / SSE upgrades from the passthrough
  mount will fail. `wrote` is read+written from `WriteHeader` and
  `Write` without synchronisation.
- **Fix**: Use `sync.Once`; pass through `http.Hijacker` /
  `http.Pusher` / `http.Flusher`.

### M9 · Mini-stream cutoff: env override of slice fields is documented as undefined

- **Where**: `internal/config/config.go` (env merge), README
- **Issue**: `OWUI_PROXY_ENGINES__TIKA__MIME_TYPES=application/pdf`
  is a single string; koanf cannot append it to a YAML slice.
- **Fix**: Document the limitation; or add a comma-separated parser
  that converts to slice for `[]string` typed keys.

### M10 · Body-size cap registered AFTER access log + metrics

- **Where**: `internal/transport/http/routes.go` (middleware order)
- **Issue**: Oversize requests are logged and counted before the
  cap kicks in.
- **Fix**: Move `BodyLimit` above `AccessLog` and `Metrics`; or
  short-circuit on `r.ContentLength > max`.

### M11 · `zerolog.AddCaller` enabled by default

- **Where**: `internal/config/config.go`
- **Issue**: `runtime.Caller` per log line; non-trivial under heavy
  access logging.
- **Fix**: Default `false`; turn on for debug.

### M12 · `recover` ordering — `RequestID` is outside it

- **Where**: `internal/transport/http/routes.go`
- **Issue**: A panic inside `RequestID` (no current cause, but
  fragile) escapes recover.
- **Fix**: Swap ordering — `Recover` first, then `RequestID`.

### M13 · Source mode silently uses default engine

- **Where**: `internal/transport/http/handlers/convert.go`
- **Issue**: `/v1/convert/source` skips MIME routing without warning;
  if `default_cee_engine=tika`, the URL goes through Tika even when
  Docling would have been correct.
- **Fix**: Document; OR allow per-source `mime` hint in the JSON
  body.

### M14 · `Async.Submit` enqueues even when the chosen engine cannot do `source`

- **Where**: `internal/transport/http/handlers/async.go`
- **Issue**: If the default engine doesn't support `http_sources`
  (Tika, Kreuzberg), the worker fails the task with a synthetic
  501. Caller paid for the round-trip and waits for failure via
  poll.
- **Fix**: Validate at submit time; reject with 400.

---

## Low

### L1 · Constant-time API-key compare leaks key length

- **Where**: `internal/transport/http/middleware/apikey.go`
- **Fix**: SHA-256 both sides first, then compare 32-byte digests.

### L2 · `validator` only catches missing URL on the default engine

- Other enabled engines without URL fail later inside `*.New`.
- **Fix**: Add `url|required_if=Enable true` validator tag.

### L3 · `var isExternalURL = defaultIsExternalURL` is package-global mutable state

- **Where**: `internal/transport/http/handlers/convert.go`
- **Fix**: Move onto the `Convert` struct.

### L4 · `panic(err)` on passthrough mount build failure at startup

- **Where**: `internal/transport/http/routes.go` (`mountPassthrough`)
- **Fix**: Return error from `NewRouter`.

### L5 · Worker queue weights are hard-coded

- **Where**: `internal/tasks/worker.go`
  (`{"default":6,"low":3,"critical":1}`)
- **Fix**: Surface in `tasks.queue_weights` config.

### L6 · `Tika.convertOne` uses `io.ReadAll` for the upstream body

- **Where**: `internal/engine/tika/tika.go`
- **Fix**: Stream-decode with `json.Decoder` so very large extractions
  do not double-buffer.

### L7 · `Health.Readiness` polls engines sequentially

- **Where**: `internal/transport/http/handlers/health.go`
- **Fix**: `errgroup.Group` parallelises the probes.

### L8 · `_ = json.Marshal` and `_ = time.Second` placeholder lines

- **Where**: `internal/transport/http/handlers/async.go`,
  `internal/app/app.go`
- **Fix**: Drop the imports.

### L9 · `BodyBytes` histogram only records `out`

- **Where**: `internal/observability/metrics.go`
- **Fix**: Also record inbound size at `BodyLimit` middleware.

### L10 · Redis URL credentials sit in process env (visible to anything that prints `Config`)

- **Where**: `internal/config/config.go`
- **Fix**: Wrap secret strings in a `Secret` type with a redacted
  `String()`/`MarshalJSON`.

---

## Nits

- **N1**: `strconvItoa` reinvented in `internal/engine/docling/docling.go` — drop, use `strconv.Itoa`.
- **N2**: `applyForwardHeaders` allowlist is a slice scan — micro-tidy with a static `switch`.
- **N3**: `noop` cleanup function is now a no-op everywhere — simplify the `(req, cleanup, err)` return shape.
- **N4**: `Engine.Pick` keeps the default name in `order` then skips it — flatten on registration.
- **N5**: asynq error handler logs without `task_id` — add `asynq.GetTaskID(ctx)`.
- **N6**: `cleanup` callback path is dead in convert handler now.
- **N7**: Sample helper `_ctxValue` in `convert.go` is unused.

---

## Roadmap aggregation

| When             | Pickup                                                                |
|------------------|------------------------------------------------------------------------|
| Pre-1.0 hardening | B1, B2, B3, H1, H2, H3, H4, H5, H6, H7, H8, H9                       |
| Post-1.0          | M1–M14                                                                |
| Continuous tidy   | L1–L10, N1–N7                                                         |
| Separate track    | Hot reload — see `docs/HOT_RELOAD_ROADMAP.md`                         |

---

## Process notes

This audit was produced by two parallel reviewers reading the
codebase statically (no test execution). Both agents had access to
`CLAUDE.md` and the live source tree. Findings cite concrete file
paths; line numbers may drift as the code evolves and should be
re-resolved at fix time.

To re-run the audit:

```sh
# from repository root
# inside Claude Code:
#  /agents → general-purpose agent → paste the prompts in
#  test/integration/scripts/security-review-prompt.txt
#  test/integration/scripts/perf-review-prompt.txt
# (the prompts that produced this document live in chat history;
#  capture in a follow-up commit if a recurring schedule is desired)
```
