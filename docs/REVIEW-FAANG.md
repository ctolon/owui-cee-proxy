# FAANG-level Multi-Agent Review — owui-cee-proxy

> Synthesised from twelve parallel reviewer agents, each anchored to a
> distinct lens (backend, QA, performance, devops/SRE, security,
> architecture, observability, API design, reliability, DX, plugin
> SDK, helm/k8s). Generated 2026-05-11, branch
> `feat/extension-routing-msg-fix`.

This document is a digest of the raw agent transcripts. Severity
follows the agents' own scale (`P0` = ship-blocker, `P1` = next
release, `P2` = backlog). Bracketed lens tags after each finding
identify which reviewer surfaced it so you can pull the full
recommendation from the agent transcript.

---

## TL;DR

The proxy's macro architecture — multi-compat adapters, facade
boundary, capabilities-driven dispatch, SSRF policy with telemetry —
is in good shape. Two classes of debt dominate the findings:

1. **Silent invariant breaks**: configuration knobs that look
   implemented but aren't (per-engine `rate_limit`, engine
   `request_timeout`), middleware wiring gaps (`Async.APIKeyHeader`
   unset), and documentation vs. code drift (`README`/`CLAUDE.md`
   diverge on routing strategy).
2. **Operational hardening**: supply-chain pinning, body-snippet
   redaction, async path Idempotency-Key, full-capture flag
   redaction allowlist, structured-log trace_id propagation.

This commit lands B1–B5 below; the rest is the prioritised backlog.

---

## In this commit

| ID | Title | Done |
|----|-------|:----:|
| B1 | Empty `engine`/`engine_url` on AccessLog when handler exits before dispatch — baseline-stamp default engine at handler entry (`convert.go`, `external.go`, `async.go`) | ✅ |
| B2 | `CLAUDE.md` rule: every commit must be preceded by `go test -race`, `go vet -tags=integration`, `golangci-lint run` | ✅ |
| B3 | Multi-stamp auth headers — `EngineConfig.AuthHeaders []AuthHeaderConfig`, dedup validator, `authutil.ApplyMulti`, passthrough multi-stamp via `FormatHeaderValuesMulti` | ✅ |
| B4 | Full request/response capture flag — `observability.upstream.full_capture` (global) + per-engine override, default OFF, redaction allowlist baked in, 64 KiB body hard cap | ✅ |
| B5 | Heavier debug-level logs — `multipart_part_received` event with size + spill-to-disk + mime-source fields | ✅ |
| F1 | Architecture P0 — `Async.APIKeyHeader` was never wired in routes.go → re-opened the resolved B3 (cross-tenant task token leak). Wired from `Security.ProxyAPIKeyHeader` | ✅ |

Verified locally: `go test -race -count=1 ./...` green;
`go vet -tags=integration ./test/integration/...` clean;
`golangci-lint run --timeout 5m` reports 0 issues.

---

## Carry-over backlog — prioritised

### Closed in `feat/security-hardening-p0`

| ID  | Lens | Resolution |
|-----|------|------------|
| C-1 | Reliability | Every adapter `Convert` now wraps `ctx` with `client.WithDeadline` so the per-engine YAML `request_timeout` reaches the outbound call. doclingexternal delegates to children — each child re-wraps. |
| C-3 | Security | `compatutil.SafeOutboundMIME` validates `f.ContentType` against the structural MIME regex before `http.Header.Set`; non-matching values fall back to `application/octet-stream`. Wired into `tika` and `external` adapters. CRLF / control-byte / parameter-bearing inputs all collapse to the safe fallback (table-driven test pins 11 inputs). |
| C-4 | Security | `validateResolvedSecrets` scans every resolved `APIKey` (singular + every `AuthHeaders` entry + `Security.ProxyAPIKeys`) for bytes outside `[0x20..0x7E] ∪ \t`. CR/LF / NUL / arbitrary control bytes fail the bootstrap with a clear `engines.<name>.api_key: resolved credential contains a non-printable byte at offset N` message. |
| C-5 | Security | `sanitizeForLog` now runs three regex passes for credential redaction: JSON-string `"keyword":"value"`, HTTP-header `Authorization:` / `Bearer <token>`, and generic `keyword=value` / `keyword: value`. Marker preserved for auditability; only value becomes `***REDACTED***`. Tests cover 6 leak shapes + the don't-overscrub case. |
| C-6 | Security | Sync `buildFileRequest` and async `payloadFromMultipart` cap non-file form-field parts at 1 MiB via `io.LimitReader(part, 1MiB+1)`. Oversized parts 400 before reaching the engine. Parity test asserts both paths reject. |
| F1 | Architecture | (Landed in the prior PR.) `Async.APIKeyHeader` wired from `Security.ProxyAPIKeyHeader` in `routes.go`. |

### Closed in `feat/timeout-hierarchy-error-envelope`

| ID  | Lens | Resolution |
|-----|------|------------|
| C-2 | Reliability | `validateTimeoutHierarchy` runs at config Load and rejects inverted timeout knobs (`max(engines.*.request_timeout) > server.request_timeout`, or `server.request_timeout + 2s > server.write_timeout`). The chi `mw.Timeout` cancel was tripping breakers on healthy-but-slow backends; the validator surfaces the misconfig at bootstrap with a precise diff. `Default()` `WriteTimeout` bumped to `5m30s` to honour the new 2 s slack. |
| C-9 | API | Every `http.Error` call in `external.go` and the async `Poll`/`Result` paths is gone — replaced with `writeJSON(w, status, respond.NewExternalError(...))` (external facade) or `respond.NewDoclingError(...)` (docling facade async). Schema-strict OpenWebUI loaders now see one shape per facade across every status code. `TestExternal_ErrorEnvelope_MissingContentType` pins it. |

### Closed in `feat/supply-chain-hardening`

| ID  | Lens | Resolution |
|-----|------|------------|
| C-7 | DevOps | Every third-party action across `ci.yaml`, `pr-checks.yaml`, `release.yaml`, `security.yaml` is now pinned to an immutable commit SHA (tag retained as a `# vX` comment for readability). `@master` is gone (trivy-action, gosec, checkov); semver-tag floats are also pinned (mathieudutour/github-tag-action, softprops/action-gh-release, amannn/action-semantic-pull-request, orhun/git-cliff-action, golangci/golangci-lint-action, erzz/dockle-action). Renovate now bumps each line atomically. |
| C-8 | DevOps | `release.yaml` gained three steps after the buildx push: (1) `sigstore/cosign-installer` v4.1.2, (2) `cosign sign --yes` against the exact pushed digest on BOTH GHCR and Docker Hub refs (keyless OIDC via the already-granted `id-token: write`), (3) `anchore/sbom-action` v0.24.0 produces a CycloneDX SBOM that `cosign attest` pins to the digest as a typed predicate. Operators verify with `cosign verify` + `cosign download attestation --predicate-type=cyclonedx`. The buildx step now writes a metadata file so the digest is canonical and immutable (no tag re-resolution race). |
| C-21 | DX | `cmd/proxy/main.go` gained `-validate` flag: loads + validates the config and exits 0 (safe to roll) / 2 (stop the rollout) without binding ports. Designed for Helm pre-upgrade hooks; smoke-tested locally against `configs/config.minimal.yaml`. The DX review's earlier "tasks.enabled=true in example.yaml without REDIS_URL" anti-example surfaces immediately with the new flag — operator-visible regression test. |

### Closed in `feat/helm-hardening`

| ID  | Lens | Resolution |
|-----|------|------------|
| C-10 | Helm | `templates/networkpolicy.yaml` now ALWAYS emits a kubelet probe allow rule (port 8080) ahead of the user-supplied `ingressFrom`. Default selector is `namespaceSelector.matchLabels.kubernetes.io/metadata.name: kube-system` (the broadly safe target since every common CNI routes its probe path there). New values knob `networkPolicy.probeSources` lets operators tighten to their exact node-pool / CNI. `ingressFrom` is independent and stacks additively. `helm template` verified — enabling networkPolicy with empty values now renders a usable policy instead of a full-deny block. |
| C-11 | Helm | `templates/deployment.yaml` `lifecycle.preStop` block (which called `/bin/sh -c "sleep 5"` against a distroless static image with no shell → exec error → no delay → in-flight RST during rolling updates) is removed. Graceful drain now relies on `terminationGracePeriodSeconds` + the Go server's SIGTERM handler (`server.shutdown_grace`, default 30 s). Operators who need a longer window bump both knobs together. `helm template` confirmed: no `lifecycle:` block in the rendered Deployment. |

### Milestone status

**All P0 carry-over items closed.** The original 11-item P0 table is exhausted across PRs #13 (security-hardening), #14 (timeout + envelope), #15 (supply-chain), and this PR (#16). What remains is the P1 / P2 backlog plus the roadmap themes. Next sprint candidates: C-12..C-32 (P1 — see the table below).

### Closed in `feat/p1-reliability-observability` (v0.5.0 + 1)

| ID  | Lens | Resolution |
|-----|------|------------|
| C-12 | Backend | `docling.convertFile` now closes `pr` with the error when `http.NewRequestWithContext` fails after the producer goroutine spawned. Pre-fix the goroutine blocked forever on `io.Copy` into a never-read pipe. Pinned by `TestDocling_ConvertFile_NoLeakOnEarlyReturn` using a NUL-byte URL trigger + `runtime.NumGoroutine` snapshot. |
| C-14 | QA | `internal/tasks/orchestrator_test.go` lifted the long-standing `t.Skip("requires Redis")` ban. New `newOrchestratorWithMiniredis` helper backs three coverage tests: `TestOrchestrator_EnqueueSaveResultRoundtrip` (full persistence lifecycle), `TestOrchestrator_TokenBindingRejectsCrossTenant` (security invariant — tenant A's token must not resolve under tenant B's apiKey or an empty caller), `TestOrchestrator_IdempotencyResolveAndRecord` (C-20 dedupe contract). asynq.Inspector path stays integration-only because miniredis can't perfectly model asynq's Lua-script queue ops. |
| C-18 | Observability | `mw.TraceFieldsFrom(ctx)` helper pulls `trace.SpanFromContext` → `(TraceID, SpanID)` strings; AccessLog `request_completed`, `engine_pick_decision`, `engine_convert_failed` all now stamp `trace_id` + `span_id` for Tempo/Jaeger joins. Defensive against nil ctx and non-recording spans (returns "" cleanly). Pinned by `TestAccessLog_TraceFieldsPropagated` + non-traced-path counterpart. |
| C-20 | API | Async `submit` honours `Idempotency-Key` header. New `Orchestrator.ResolveIdempotent` + `RecordIdempotent` helpers use Redis `SETNX` keyed on `sha256(apiKey)` + `sha256(idem)`. TTL matches `ResultTTL`. Retries within the window resolve the prior token without re-enqueueing. SETNX-loser sees winner's token. Cross-tenant isolation enforced by apiKey-prefix in the key. Pinned by `TestOrchestrator_IdempotencyResolveAndRecord` (5 sub-cases). |

### Closed in `feat/p1-perf-tracing-contracts`

| ID  | Lens | Resolution |
|-----|------|------------|
| C-15 | QA | `enginetest.RunContractTests` tightened: URL idempotency, every advertised facade exercised (not just the first), Health on cancelled ctx MUST surface `context.Canceled` / `context.DeadlineExceeded` / a "context" substring (was: any non-empty error string — a tautology), Convert response StatusCode > 0 on nil err, response Body safe to `Close()` twice. Earlier version was "a probe, not a contract" — almost any adapter return shape passed. |
| C-17 | Perf | `staticRegistry.phases` pre-computed at `NewRegistry` time; `PickRouteVerbose` reads the struct field instead of allocating a `[]dispatchPhase` slice literal per request. Eliminates the per-request heap escape on the routing critical path. |
| C-19 | Observability | `internal/httpclient/client.go` wraps the per-engine `*http.Transport` with `otelhttp.NewTransport` so outbound requests carry W3C `traceparent` / `tracestate` / `baggage` headers; backends continue the proxy's trace span tree end-to-end. Propagator passed explicitly (W3C TraceContext + Baggage) rather than reading the global, so the wrap fires correctly in unit tests too. New `Client.InnerTransport()` accessor for the unwrapped `*http.Transport` so existing tests can introspect transport knobs. Pinned by `TestNew_OtelHTTPTransportPropagatesTraceparent`. |

### Closed in `feat/p1-architecture-plugin-sdk`

| ID  | Lens | Resolution |
|-----|------|------------|
| C-13 | Backend | `WithMimeSource` renamed to `RecordMimeSource` and now returns nothing. The verb prefix signals the side-effect contract explicitly; the old name + returned-ctx signature lied (the helper mutates the requestMeta pointer in ctx, callers that ignored the returned ctx still saw the mutation — indistinguishable from a real ctx-chaining helper). Two call sites updated. |
| C-25 | Architecture | `mountEnginePassthrough` now passes the per-engine raw `*http.Transport` (sourced from the new `Client.InnerTransport()`) to `reverseproxy.New` instead of `nil` (which fell back to `http.DefaultTransport` shared across every engine). New `RouterDeps.EngineTransports map[string]*http.Transport` carries the mapping from the composition root. Per-engine isolation invariant restored. |
| C-26 | Plugin SDK | Validator tag for `EngineConfig.CompatType` switched from a hand-maintained `oneof=docling external docling-external tika` literal to a custom `compat_type_valid` validator registered at `Validate()` time. The accepted set comes from the new `compatTypes` slice — adding a new compat_type is now `const + slice entry + acceptedFacades switch arm`, with the validator following automatically. New exported `CompatTypes()` helper returns a defensive copy for adapter SDK + test alignment. |
| C-27 | Plugin SDK | New `TestCompatTypesAndAcceptedFacadesAlignWithAdapterCapabilities` in `internal/app/app_test.go`: instantiates every registered adapter via `newAdapterByCompat` and asserts `adapter.Capabilities().Facades` matches `config.AcceptedFacades(compatType)`. Two sources of truth still exist (they serve different lifecycle stages — validator at config-Load vs registry at dispatch) but a drift in either now fails CI. |
| C-29 | Helm | `deployments/kubernetes/base/kustomization.yaml` no longer lists `secret.example.yaml` in its `resources:` block. A GitOps pipeline with `prune=true` would previously materialise a live `Secret` containing the literal `REPLACE_ME` placeholder. Inline comment documents the intent: apply the example directly for scaffolding only; production operators supply real values via SealedSecrets / ExternalSecrets. |

### Closed in `feat/p1-perf-headers-observability`

| ID  | Lens | Resolution |
|-----|------|------------|
| C-16 | Perf | `spoolPart` now grows a `bytes.Buffer` from a 64 KiB bootstrap up to `threshold+1`, replacing the pre-allocated `make([]byte, 8MiB+1)` that ran on every upload regardless of size. Peak alloc for sub-threshold files is now ~next_pow2(filesize) instead of a flat 8 MiB; 100 concurrent <1 MiB uploads now total ~6.4 MiB of buffer alloc at peak instead of 800 MiB. |
| C-30 | API | New `copyResponseHeaders` helper applies an allowlist (`Content-Type`, `Content-Length`, `Content-Encoding`, `Content-Language`, `Etag`, `Last-Modified`, `Cache-Control`, `Expires`, `Vary`, `X-Request-Id`) plus a belt-and-braces deny list (`Set-Cookie`, `Set-Cookie2`, `Server`, `X-Powered-By`, `X-Aspnet-Version`, `X-Aspnetmvc-Version`). CR/LF-bearing values are dropped from allowlisted headers too. Wired into both convert.go and external.go. Pinned by two unit tests. |
| C-32 | Observability | New `logEnginePickDecision` helper centralises the structured field set of `engine_pick_decision`; convert.go + external.go now call ONE function instead of duplicating the emit block. Field set expanded with `facade`, `mime_declared`, `mime_resolved`, `mime_source`, `file_ext`, `file_count`, `compat_type` (reserved for future engine-SDK extension). Adding a new attribution field is now a single-file change. |

### Closed in `feat/p1-async-reliability`

| ID  | Lens | Resolution |
|-----|------|------------|
| C-22 | Reliability | New `Health.Required map[string]struct{}` carries the load-bearing probe names (default engine + Redis when tasks are enabled, populated by `requiredReadinessNames`). When set, `/readyz` flips to 503 ONLY if a required probe fails — non-required engines surface as `"degraded": [...]` in the body without ejecting the pod. Empty Required preserves legacy all-or-nothing behaviour. Pinned by two new tests (tiered pass + required fail). |
| C-23 | Reliability | `Orchestrator.Enqueue` now uses `redis.Pipeline()` twice: once for the N blob SETs (1 RTT instead of N) and once for the post-Enqueue token-binding + queue-mapping pair (1 RTT instead of 2). Net: a 3-blob async submit drops from 6 sequential Redis round-trips to 3 (1 for blobs, 1 for asynq, 1 for token+queue). Slow-Redis no longer single-threads the request goroutine. |
| C-28 | Security | API-key fingerprints now use HMAC-SHA-256 with a per-process pepper (configurable via `security.proxy_api_key_fingerprint_pepper_env`) instead of plain SHA-256. Orchestrator carries the pepper via `WithFingerprintPepper`; empty pepper falls back to the legacy plain SHA-256 path for backward-compat with existing deployments. A Redis exfil can no longer recover API keys via offline rainbow-table — the attacker also needs to steal the pepper. Pinned by `TestOrchestrator_FingerprintPepperHMAC`. |

### Closed in `feat/p1-rate-limit-fallback`

| ID  | Lens | Resolution |
|-----|------|------------|
| C-24 | Architecture | `engines.<n>.rate_limit` no longer silent dead code. New `RateLimitedTransport` (golang.org/x/time/rate) wraps every per-engine `*http.Transport` when `RPS > 0`. Sits below otelhttp + InstrumentedTransport so the limiter's `Wait` time is captured in the trace span + the upstream-duration metric. Empty / zero config preserves the previous unthrottled behaviour. |
| C-31 | Reliability | Single-step engine fallback. New `routing.fallback.enabled` knob (default false). When ON, a primary engine's 5xx or transport-level error triggers a retry against the registry's default engine (when primary != default). Body rewind via `io.Seeker`; any non-seekable body aborts the fallback so we never half-send. Pinned by `TestConvert_FallbackOn5xxRetriesAgainstDefault` + the disabled-path counter test. Multi-tier chain explicitly out-of-scope — covers the most common case (flaky non-default → fall back to default) without designing a fallback-chain config schema. |

### Milestone status

**All carry-over P1 items closed.** The 14-item P1 backlog from
the original FAANG review is exhausted across PRs #18 (C-12, C-14,
C-18, C-20), #19 (C-15, C-17, C-19), #21 (C-13, C-25, C-26, C-27,
C-29), #22 (C-16, C-30, C-32), #23 (C-22, C-23, C-28), and this
PR (C-24, C-31). Combined with the v0.5.0 P0 milestone, the
original 25-item backlog is fully retired.

### P1 (release after P0) — remaining

(Nothing left in this band; future work would draw from the P2
list + roadmap themes.)

| ID  | Lens | Finding | Recommended action |
|-----|------|---------|--------------------|
| C-22 | Reliability | `/readyz` is all-or-nothing — one flaky engine kills the whole pod. | Tier the probe: default-engine + Redis required, others surface degraded. |
| C-23 | Reliability | Async `Enqueue` does 4 sequential Redis round-trips inside the request goroutine. Slow Redis blocks every async submit. | Pipeline via `redis.MULTI`; or single Lua script. |
| C-24 | Architecture | `cfg.engines.<n>.rate_limit` is in the schema, defaulted, **never used**. `docs/ARCHITECTURE.md` lies about it. | Implement per-engine `golang.org/x/time/rate` limiter, or delete the field. |
| C-25 | Architecture | Per-engine passthrough mounts pass `transport=nil` → use `http.DefaultTransport`, violating the per-engine isolation invariant. | Pass the engine's tuned `*http.Transport` from `httpclient.New`. |
| C-26 | Plugin SDK | `compat_type` literal duplicated in `config.go:74` (const), `:214` (validator tag), `app.go:356` (switch). Adding one is a 4-file change. | Derive the validator list from the consts. Or registry-pattern `engine.Register("marker", marker.New)` in `init()`. |
| C-27 | Plugin SDK | `acceptedFacades` (`config.go:850`) is a **second source of truth** for compat→facade mapping. Adapter `Capabilities()` is the first; they can drift. | Drop `acceptedFacades`; build the map at startup by instantiating each adapter. |
| C-28 | Security | Async result endpoint binds to API-key fingerprint via plain SHA-256 of the key. Redis-exfil + rainbow table = key recovery. | HMAC-SHA-256 with a per-process pepper (`security.proxy_api_keys_pepper_env`). |
| C-29 | Helm | `secret.example.yaml` is listed in `kustomization.yaml::resources`, so GitOps apply creates a literal `REPLACE_ME` Secret. | Remove from resources list; keep file as doc only. |
| C-30 | API | Engine response headers (`Server`, `X-Powered-By`, `Set-Cookie`) leak through `convert.handle` / `external.Process` verbatim. | Allowlist response headers; always strip `Set-Cookie`. |
| C-31 | Reliability | No engine fallback chain. A 5xx from one engine kills the whole MIME class even when sibling engines are healthy. | Add `routing.fallback` ordered candidates; integrate with breaker state. Requires C-20 first. |
| C-32 | Observability | `engine_pick_decision` debug event lacks `compat_type`, `facade`, `mime_declared`, `mime_resolved`, `mime_source`, `file_ext`, `trace_id`. Duplicated in `convert.go` ↔ `external.go`. | Promote to `mw.LogEnginePick(ctx, logger, decision)`; expand fields. Mirror as `engine_pick_total{facade, engine, pick_source}` counter. |

### Closed in `feat/p2-quick-wins` (v0.7.0 + 1)

| ID  | Lens | Resolution |
|-----|------|------------|
| C-34 | Helm | `templates/deployment.yaml` now plumbs `topologySpreadConstraints`, `nodeSelector`, `tolerations`, `affinity` through to the Pod spec. `values.yaml` ships an opt-in stub with the canonical `maxSkew: 1, topologyKey: hostname` recipe in a comment. Multi-replica deployments can finally enforce zone/node spread the PDB was already gating on. |
| C-39 | Observability | `Recover` middleware now emits the panic stack via `Str(...)` instead of `Bytes(...)` (the latter base64-encoded the stack in JSON, making it unreadable). Adds a new `owui_cee_proxy_panics_total{path}` counter wired through `PanicRecorder` interface + composition-root adapter. Path label is bounded by chi route patterns; the literal `"unknown"` covers the rare panic-before-routing case. |
| C-42 | Helm | `values.yaml` extended with stubs for every YAML knob added in v0.5.0–v0.7.0: `routing.strategy`, `routing.fallback.{enabled,max_attempts}`, `engines.<n>.extensions`, `engines.<n>.auth_headers`, `engines.<n>.rate_limit`, `mimedetect.extension_overrides`, `security.proxy_api_key_fingerprint_pepper_env`. Each carries an inline comment pointing at the version it landed in. |
| C-44 | Security | `validateSources` now mutates each `engine.HTTPSource` after a successful resolve: URL rewritten to use the resolved IP literal (IPv6 bracketed when needed), original hostname preserved in `Headers["Host"]`. The engine backend dialing the rewritten URL can no longer fall through to a fresh DNS lookup — the TOCTOU window between validation and dial is closed. Caller-supplied Host headers are preserved (operators driving a CDN front keep control). Pinned by 4 unit tests (IPv4 + IPv6 + caller-Host + rejection-propagation). |

### P2 (backlog) — remaining

A subset, ordered by likely ROI:

- **C-33** `EnginePathsConfig` is a per-compat grab-bag struct that N×M-explodes; replace with `Paths map[string]string` per-engine. [Plugin SDK]
- **C-35** No OpenAPI/AsyncAPI spec — the single largest UX gap for client teams. [API design]
- **C-36** No `values.schema.json` for the Helm chart; operator typos reach the cluster silently. [Helm]
- **C-37** Body-limit middleware (`bodylimit.go`) has zero tests; ratelimit + timeout middleware same. [QA]
- **C-38** Tasks duplicate spool implementation (`tasks/spool.go` vs `handlers/spool.go`) — unify into `internal/spool/`. [Backend]
- **C-40** Composition root `app.go` is 444 lines and growing; extract observability adapters + bootstrap loggers. [Architecture]
- **C-41** `docs/README.md` routing section still describes v0.2.x; doesn't mention `routing.strategy` or `extensions`. [DX]
- **C-43** Gateway API overlay has HTTP-only listener (no TLS); ingress-nginx overlay has TLS — parity gap. [Helm]
- **C-45** Async path supports only docling facade (`/v1/convert/*/async`); external `/process/async` is missing — facade asymmetry. [API design]

---

## Roadmap themes (6–12 month horizon)

These are the cross-cutting initiatives the agents converged on. Each
ships behind multiple individual P0/P1 items.

1. **Plugin SDK extraction** — promote `internal/engine` +
   `enginetest` + `breaker` to `pkg/engine` so third-party authors
   can write out-of-tree adapters with a versioned API. Closes C-26,
   C-27, C-33.
2. **gRPC adapter family (`compat_type: grpc`)** — MinerU-direct,
   Paddle, modern inference services. Drives `EngineCapabilities`
   extension: `StreamingResponse`, `MultiFile`, `AsyncNative`,
   `OCR`, `MaxFileSize`.
3. **Multi-tenant credentials** — per-API-key auth header rewriting.
   Today every engine has one credential set; B2B needs
   tenant-scoped credentials, which compose naturally with the
   multi-stamp work landed here.
4. **OpenAPI v3 spec + CI breaking-change gate** (`oasdiff`).
   Auto-generated Go + Python client SDKs with `Idempotency-Key`
   retry helpers.
5. **`-validate` CLI + Helm pre-upgrade validation hook** — closes
   the rollback gap. Operator changes `routing.strategy`, runs
   `helm test --dry-run`, gets fast-fail before the cluster takes
   the bad config.
6. **SBOM + cosign + SLSA provenance** in the release pipeline.
   Closes the supply-chain risks (C-7, C-8).
7. **Audit log channel** for the full-capture flag landed here.
   Today it emits onto the primary log stream; the target is a
   separate sidecar Unix socket so operators can route it to cold
   storage without polluting the main log shipper.
8. **Tracing-aware backpressure** — couple OTel span attributes
   (`upstream_in_flight`, `upstream_latency_p99`) to engine-level
   rate-limiter throttling. Pairs with C-24.
9. **Adapter `request_timeout` enforcement** — single-PR fix for
   C-1; gates every later reliability initiative.
10. **WASM-based custom routing predicates** — operators write
    `predicate.wasm` consuming `(mime, filename, headers,
    content_length)` → `engine_name`. Late v1.0 / v2.0 lever; pays
    back when the YAML routing schema grows past five strategies.

---

## Lens-by-lens highlights

Below is a one-liner per reviewer for cross-reference. The full
transcript per agent is available under
`/tmp/claude-1000/-home-ctolon-owui-cee-proxy/<session>/tasks/`.

| Lens | Top-of-report finding |
|------|-----------------------|
| Backend / Go idioms | docling pipe goroutine leak on early error path (C-12); context-setter contract is mutating-not-returning (C-13). |
| QA / Test strategy | Wire `miniredis` and de-skip the `tasks` lifecycle suite (C-14); `RunContractTests` is too lax (C-15). |
| Performance | Spool always allocates 8 MiB regardless of size (C-16); `strategyPhases` slice escapes per request (C-17). |
| DevOps / SRE | Actions pinned to `@master` (C-7); release pushes are unsigned (C-8); no `-validate` flag (C-21). |
| Security (OWASP+STRIDE) | Multipart form-field DoS (C-6); MIME forward injection (C-3); secrets in body snippets (C-5). |
| Architecture | Async APIKeyHeader unwired (F1, landed); `rate_limit` silent dead code (C-24); passthrough transport not isolated (C-25). |
| Observability | `trace_id` missing from every log (C-18); outbound transport not OTel-wrapped (C-19); `engine_pick_decision` duplicate + thin (C-32). |
| API design | External facade error envelopes drift (C-9); no `Idempotency-Key` (C-20); response header leakage (C-30). |
| Reliability | Adapter `request_timeout` is dead code (C-1); `Server.WriteTimeout` truncates streams (C-2); no fallback chain (C-31). |
| Documentation / DX | README ↔ CLAUDE.md drift on routing strategy (C-41); `-validate` flag missing (C-21); error messages leak validator playground noise. |
| Plugin SDK | Three-file `compat_type` change (C-26); `acceptedFacades` second source of truth (C-27); contract test is a probe (C-15). |
| Helm / K8s | NetworkPolicy default deny-all (C-10); distroless `preStop` exec failure (C-11); `secret.example.yaml` listed as live resource (C-29). |

---

## How to consume this document

- **Operators**: skim TL;DR + "In this commit", read the P0 table to
  understand what is shipping next.
- **Maintainers**: the P0/P1 tables ARE the milestone planning unit.
  Each row corresponds to a single PR-sized change.
- **Third-party adapter authors**: the Plugin SDK section under
  "Roadmap themes" is the most relevant.
- **Security auditors**: the security lens findings (C-3 through C-6,
  C-28, C-44) are the active threat surface; the rest is
  defense-in-depth.

Updates to this document follow the same review-then-merge flow as
the code. When closing an item, move it from a P-band table into a
"Closed in" section with the release tag.
