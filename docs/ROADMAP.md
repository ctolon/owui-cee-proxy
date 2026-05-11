# Roadmap

Long-running initiatives that exceed sprint scope. Each entry is
sized for a self-contained project plan that can be picked up
independently. All P0/P1 items and 13 of 14 P2 items from
[`REVIEW-FAANG.md`](./REVIEW-FAANG.md) closed by v0.8.0; this doc
captures the work explicitly *not* attempted in that sprint cadence.

The chunks are ordered by likely operator value, not by
dependency. Pick the one that scratches the loudest itch and
treat it as a multi-PR initiative.

---

## 1. C-35 — OpenAPI v3 + AsyncAPI spec (the single largest UX gap)

**Why it matters.** Today every client team writes the proxy's
wire shape by reading handler code or tcpdumping a sample
request. The two facades, the four async endpoints, the source-
mode JSON shape, the error envelopes, and the engine response
header allowlist are all undocumented at machine-readable level.
That blocks:

- Auto-generated Go / Python / TypeScript client SDKs.
- Breaking-change gates (`oasdiff`-style) on PRs that touch
  request/response shapes.
- Integration test fixtures generated from the spec.
- Documentation portals (Redoc, Swagger UI, Stoplight) that
  could ship inside the chart as a sidecar.

### Scope

Two specs, intentionally:

1. **OpenAPI v3 (`docs/openapi.yaml`)** covers the synchronous
   surface and the operational endpoints:
   - `POST /v1/convert/file` — multipart upload, docling
     facade. Document the engine-pick decision flow (MIME +
     filename → `engine_pick_decision`) as descriptions on
     the response headers.
   - `POST /v1/convert/source` — JSON body with `http_sources[]`.
     Pin the SSRF behaviour as response codes.
   - `PUT /process` — raw body, external facade. Document
     `Content-Type` + `X-Filename` semantics, response envelope.
   - `POST /v1/convert/file/async` + `/source/async` +
     `POST /process/async` — submit endpoints with `Idempotency-Key`
     header.
   - `GET /v1/status/poll/{id}` — facade-agnostic.
   - `GET /v1/result/{id}` — facade-agnostic; pin the H4 content
     allowlist + nosniff behaviour.
   - `GET /healthz` / `/readyz` — payload shape (tiered probe
     status) + status code semantics.
   - `GET /metrics` — out of scope (Prometheus text format,
     better served by a `metrics_reference.md` listing every
     `owui_cee_proxy_*` series with labels + help text).
   - `GET /version` — `version-info` schema.

2. **AsyncAPI v3 (`docs/asyncapi.yaml`)** covers the internal
   asynq queue:
   - Channel: `owui-cee-proxy/queue/{queueName}` (default, low,
     critical).
   - Message: `tasks.Payload`.
   - Reply correlation: TaskID → `/v1/result/{id}`.

### Critical decisions

- **Schema source-of-truth.** Either:
  - **(a)** Hand-write the YAML and run a CI job that asserts
    the Go types match (struct tag → schema). Simple, error-
    prone, but introspectable.
  - **(b)** Generate the schema from Go via `kin-openapi` or
    `swag`. Requires annotations on every handler; introduces
    a generator-step in `make build`. More mechanical, less
    flexible for human-curated docs.

  Recommendation: **(a)** for the first cut. The proxy's
  surface is ~10 endpoints; manual maintenance is tractable.

- **Schema reuse.** Multipart envelopes for the facade endpoints
  share a substantial `options{}` map (`engines.<n>.forward_options`).
  Use `$ref`-shared `Options` component.

- **Error envelopes.** The docling facade and external facade
  emit DIFFERENT envelope shapes (see `respond/`). Document each
  via `oneOf` on the error response.

- **Engine matrix.** Don't document per-engine paths — those are
  operator overrides (`paths.<key>`). Document the facade
  contract; whichever engine answers must honour it.

### Validation gates

- `oasdiff breaking` job in CI: any PR that touches `docs/openapi.yaml`
  must run against the previous release's spec and either be
  non-breaking or carry an explicit `BREAKING CHANGE:` note in
  the PR body.
- `spectral lint docs/openapi.yaml` for style consistency.
- A Go test (`api_contract_test.go`) that spins up the handler
  chain with a mock registry and asserts each documented status
  code is actually produced for a representative request.

### Estimate

3–5 working days for the first cut (spec drafting + handler
audit + CI integration). Adds ~1500 lines of YAML.

### Pre-work that helps

- Pin a `respond/` package audit: confirm every error path
  routes through `respond.NewDoclingError` / `NewExternalError`
  (not raw `http.Error`). Two passes are needed before the spec
  describes the envelopes accurately.
- Tag every handler-emitted log event in `docs/observability.md`
  (a small doc, not currently present) so the spec's
  description fields can cross-reference them.

---

## 2. Roadmap themes (multi-quarter)

These are cross-cutting initiatives. Each one is large enough
that it should run as its own multi-PR project, not a single
sprint. Sequencing matters — Plugin SDK and gRPC family should
land together; multi-tenant credentials depends on the auth
work landed in v0.5.0; OpenAPI is gated on C-35 above.

### 2.1 Plugin SDK extraction (`internal/engine` → `pkg/engine`)

**Goal.** Make `Engine` / `Registry` / `Capabilities` / `breaker` a
public, versioned API so third-party authors can write
out-of-tree adapters.

**Closes the residual hazards** of C-26 / C-27 / C-33 (now
landed) by promoting them into a contract operators can
depend on.

#### Required surface

```
pkg/engine/
  engine.go        — Engine, ConvertRequest, ConvertResponse,
                     FileBlob, HTTPSource, Facade, PickHint,
                     PickSource, EngineCapabilities.
  registry.go      — Registry interface, RegistryEntry, NewRegistry.
  routing.go       — RoutingStrategy.
  errors.go        — sentinel errors with stable messages.

pkg/engine/contract/
  contract.go      — currently enginetest.RunContractTests; rename
                     + harden assertions (every adapter MUST cover
                     N% of the test matrix).

pkg/engine/breaker/   — sony/gobreaker thin wrapper.
pkg/engine/respond/   — JSON envelope helpers.
```

#### Tasks

1. **Move types** with a redirection shim in `internal/engine/`
   so the existing call sites compile during the migration.
2. **Add SDK semver tag** on `pkg/engine` independent of the
   binary version. (Pre-v1 caveat: the binary can keep its own
   semver; the SDK starts at `v0.1.0`.)
3. **Adapter registration via init()** — replace the switch in
   `internal/app/app.go::newAdapterByCompat` with a registry
   that maps `compat_type` string → factory function. Adapters
   call `engine.Register("docling", docling.New)` in `init()`.
   Closes C-26 (the four-file change for a new compat is gone).
4. **Validator integration** — `validator.New().RegisterValidation("compat_type", ...)`
   reads the live registered set. Operators with a third-party
   adapter linked in see it accepted automatically.
5. **Example out-of-tree adapter** under `examples/`. Tutorial in
   `docs/SDK-AUTHORING.md`.

#### Estimate

2 weeks. Most of the cost is the test migration: every
contract test must pass against the same suite the in-tree
adapters use.

---

### 2.2 gRPC adapter family (`compat_type: grpc`)

**Goal.** Reach modern inference services natively (MinerU,
Paddle, custom NVIDIA Triton) without the HTTP-to-gRPC
gateway layer.

#### Scope

- New compat package `internal/engine/compat/grpc/`.
- Drives `EngineCapabilities` extension: `StreamingResponse bool`,
  `MultiFile bool`, `AsyncNative bool`, `OCR bool`, `MaxFileSize int64`.
- Config schema: `engines.<n>.grpc.{endpoint, tls, codec,
  service_descriptor}`. The service descriptor (`.proto`) lives
  alongside the engine config or is fetched via gRPC reflection.
- Streaming reply support: the proxy's handlers need a
  `streamReply` code path for engines that produce results
  incrementally. Today the handler signature returns
  `*engine.ConvertResponse` (Body=`io.ReadCloser`), so streaming
  composes naturally — the gRPC adapter wraps the stream as a
  reader.

#### Anti-goals

- gRPC-Web in the OUTBOUND direction (engines speak full gRPC).
- Long-lived bidi streams (one-shot request/response only).
- Auto-discovery of services beyond reflection.

#### Estimate

2–3 weeks. The hardest part is `EngineCapabilities` extension
without breaking the existing `internal/engine/enginetest`
contract for HTTP adapters — the contract test needs a fork.

---

### 2.3 Multi-tenant credentials

**Goal.** Per-API-key auth header rewriting. Today every engine
has ONE credential set; B2B / multi-tenant deployments need
tenant-scoped credentials.

#### Scope

- New config block:

  ```yaml
  security:
    tenants:
      tenant-a:
        api_keys_env: TENANT_A_KEYS
        engine_credentials:
          main-docling:
            api_key_env: TENANT_A_DOCLING_KEY
            auth_headers:
              - header: X-Tenant
                api_key_env: TENANT_A_TENANT_TOKEN
  ```

- Inbound APIKey middleware augmented with tenant identification
  (header `X-Tenant-Id` OR derived from API-key prefix).
- Outbound auth-stamping (`authutil.Apply`) consults the tenant's
  override before falling back to the engine's default.

#### Anti-goals

- Tenant isolation at the network layer (egress firewall per
  tenant) — out of scope; that's the operator's NetworkPolicy.
- Quota per tenant — orthogonal; the per-engine rate limiter
  landed in v0.7.0 can be extended via composition.

#### Estimate

1.5 weeks. The multi-stamp auth work landed in v0.5.0 already
exposes the per-engine seam (`AuthHeaders []AuthHeaderConfig`).
This adds an inbound-driven *which set* selection.

---

### 2.4 `-validate` CLI + Helm pre-upgrade hook

**Goal.** Catch bad config before a rollback is needed.
Operator runs `helm upgrade --dry-run`, gets fast-fail on
typos / wrong refs / unreachable backends. Closes the rollback
gap C-21 flagged.

#### Scope

- `cmd/proxy --validate /path/to/config.yaml`:
  - Parses the YAML through the same `koanf.Load` path
    the binary uses.
  - Runs all validators (struct tag + custom validators).
  - Resolves env-var-borne secrets ONLY if present (warns
    otherwise rather than failing).
  - Attempts a synchronous health probe against each declared
    engine URL (configurable timeout, default 5s) so DNS / TLS
    misconfig fails the validation.
  - Exits 0 on success; emits a structured JSON error report
    on stderr otherwise.

- Helm chart: new `templates/tests/test-config-validate.yaml`
  pod with the test annotation. `helm test` after upgrade runs
  the validator inside the cluster against the rendered
  ConfigMap.

- Pre-commit hook recipe in `docs/CI.md` so operators using
  GitOps catch typos at PR time, not at deploy time.

#### Estimate

1 week. The validation logic mostly exists; this is plumbing.

---

### 2.5 SBOM + cosign + SLSA provenance

**Goal.** Close the supply-chain risks (C-7, C-8). Status:
**partially landed** in v0.7.0 (cosign keyless signing +
CycloneDX SBOM attestation). Remaining work is SLSA
provenance attestation + the verification side.

#### Remaining tasks

- Add `provenance: mode=max` to the docker buildx step in
  `release.yaml`.
- Sigstore policy controller manifest in `deployments/kubernetes/security/`
  (Kyverno or Gatekeeper) that REQUIRES a valid cosign signature
  + SLSA provenance for the proxy image. Document the
  verification command in `docs/SECURITY.md`.
- Reproducible-build CI job: same source tree on two runners
  should yield the same image digest (modulo timestamps —
  use `SOURCE_DATE_EPOCH` from the git commit).

#### Estimate

3–4 days.

---

### 2.6 Audit log channel

**Goal.** The full-capture flag landed in v0.5.0 emits onto the
primary log stream. Target: a SEPARATE sidecar Unix socket so
operators can route the audit stream to cold storage without
polluting the main shipper.

#### Scope

- `observability.audit.{enabled,socket_path,buffer_bytes}`
  config block.
- New `internal/observability/audit/` package implementing a
  zerolog writer pointed at a Unix domain socket.
- `mw.FullCapture` writes to the audit writer when
  configured; falls back to the primary logger otherwise
  (current behaviour).
- Helm chart sidecar (opt-in via `audit.enabled`): a small
  `socat` or `fluent-bit` container that reads from the socket
  and forwards to S3 / GCS / Azure Blob.

#### Estimate

1 week.

---

### 2.7 WASM routing predicates

**Goal.** Operator-supplied routing logic compiled to WASM that
the registry consults INSTEAD of (or BEFORE) the MIME/extension
strategies.

**Why later, not sooner.** This is a power-user feature with
significant complexity (WASM runtime, sandboxing, observability
of arbitrary code). The four-strategy enum landed in v0.5.0
covers ~95% of operator needs. Reach for WASM only when an
operator demonstrably needs branching on a signal the
declarative strategies can't express (e.g., "route by header
value", "route by request rate per CIDR").

#### Sketch

- New package `internal/engine/predicates/wasm/` using
  `wazero` (no CGO).
- Config: `routing.predicates: [path/to/predicate.wasm]`.
- Predicate signature (WASM exports):
  `decide(facade, mime, filename, headers_json) -> engine_name`.
- Each predicate gets a CPU + memory budget enforced by the
  runtime.

#### Estimate

3+ weeks. Defer until a concrete operator use-case lands.

---

## Sequencing recommendation

If the team wants to allocate ~6 months of work, this is the
ordering that produces the most operator value with the
least drag from breaking changes:

1. **C-35 OpenAPI/AsyncAPI** (3–5 days) — unblocks client SDKs.
2. **2.4 `-validate` CLI + Helm test** (1 week) — closes the
   rollback gap, low risk.
3. **2.1 Plugin SDK extraction** (2 weeks) — sets up 2.2 and
   future compat additions.
4. **2.2 gRPC adapter family** (2–3 weeks) — needs 2.1.
5. **2.3 Multi-tenant credentials** (1.5 weeks) — independent.
6. **2.5 SBOM/SLSA polish** (3–4 days) — independent.
7. **2.6 Audit log channel** (1 week) — independent.
8. **2.7 WASM predicates** — defer until demand signal.

Total: ~12 weeks of engineering time for items 1–6, leaving
2.7 in the parking lot.
