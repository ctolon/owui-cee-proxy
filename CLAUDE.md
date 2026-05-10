# CLAUDE.md — Project Guide

> Read this first. It encodes the load-bearing decisions that aren't
> obvious from code.

> Companion docs:
> - [`docs/ARCHITECTURE.md`](./docs/ARCHITECTURE.md) — system architecture, request lifecycle, decision log
> - [`docs/BRANCHING.md`](./docs/BRANCHING.md) — `dev` ↔ `main` flow, semver, release contract
> - [`docs/REVIEW.md`](./docs/REVIEW.md) — security/perf review, prioritised follow-ups
> - [`docs/HOT_RELOAD_ROADMAP.md`](./docs/HOT_RELOAD_ROADMAP.md) — nginx-style reload plan

## What this is

A Go 1.26.1 reverse proxy that sits between OpenWebUI 0.9.2 and three
content extraction backends:

| Engine    | Default URL                  | Endpoint hit by adapter                              | Notes |
|-----------|------------------------------|------------------------------------------------------|-------|
| Docling   | `http://docling-serve:5001`  | `POST /v1/convert/{file,source}` (native facade)     | streaming passthrough; response normalised for `md_content: null` (OpenWebUI 0.9.2 strict Pydantic). |
| Tika      | `http://tika:9998`           | `PUT /tika/text` per file, then aggregate            | response transformed into Docling envelope via `tika/transform.go`. |
| Kreuzberg | `http://kreuzberg:8000`      | `POST /v1/convert/file` (native docling-compat)      | Kreuzberg ships an OpenWebUI/docling-serve compatible facade tagged `openweb`; we passthrough — **no response shaping**. |

The proxy exposes:

1. **Docling-compatible facade** at `/v1/convert/*`. Engine selection
   is **MIME-driven**: the first uploaded file's `Content-Type` is
   matched against each non-default enabled engine's
   `engines.<name>.mime_types` list (exact matches and `type/*`
   wildcards). The first match wins.
   `routing.default_cee_engine` is the catch-all when no list
   matches (or when the request is a `/v1/convert/source` JSON call,
   which has no MIME hint without dereferencing the URL). Tika and
   Kreuzberg responses are transformed to the Docling shape so
   OpenWebUI doesn't need to know which engine actually ran. Async
   siblings live at `/v1/convert/{file,source}/async` with
   `/v1/status/poll/{id}` and `/v1/result/{id}` (Redis-backed via
   asynq).
2. **Native passthrough mounts** at `/docling/*`, `/tika/*`,
   `/kreuzberg/*` — pure streaming reverse proxy onto the matching
   backend. Used for native-feature access and for testing each engine
   independently.
3. **Operational endpoints**: `/healthz`, `/readyz`, `/metrics` (Prom),
   `/version`.

## Load-bearing invariants — DO NOT VIOLATE

1. **Stateless**. The proxy itself stores nothing in process memory
   beyond config and connection pools. Async tasks live in Redis;
   any replica can serve any task.
2. **Streaming, with one deliberate exception**. The outbound request
   body to the engine is `io.Pipe` + `mw.CreatePart` (streaming). The
   inbound multipart body, however, is buffered into memory part-by-
   part inside `handlers/buildFileRequest`. Reason: `multipart.Reader`
   drains the previous part the moment `NextPart()` is called, so we
   must read each part fully before requesting the next. Doing
   otherwise sends empty bodies to the engines (silent data loss). The
   per-request body cap (`server.max_body_bytes`, default 500 MiB)
   bounds this allocation. `tasks/payload.go` buffers as well because
   asynq payloads must be serialisable.
3. **Secrets never live in YAML**. The YAML names the env var
   (`api_key_env`); `config.resolveSecrets` looks it up at startup.
4. **Engine pluggability**. Adding a new engine MUST require zero
   changes to handlers, routes, or middleware. Touch only:
   - new package under `internal/engine/<name>/`
   - new field in `EnginesConfig` (config.go); include
     `mime_types` so MIME-based routing can dispatch to it
   - one branch in `app.buildRegistry` (composition root) that
     constructs a `RegistryEntry` carrying the configured `MimeTypes`
   - one branch in `routes.apiKeyHeaderFor` if its auth header differs
5. **Liskov via contract test**. Every adapter is verified by
   `internal/engine/enginetest.RunContractTests`. New adapters MUST
   wire that suite into their `*_test.go`.
6. **Logs always carry `engine`, `engine_url`, `filename`** for
   convert-path requests. Operators rely on these to attribute a
   particular file upload to a particular backend (the user's primary
   stdout requirement).
7. **Shutdown order**: worker → orchestrator close → trace flush →
   server graceful Shutdown. See `internal/app/app.shutdown`.

## Commands

```sh
make build              # CGO_ENABLED=0 binary into bin/
make test               # unit tests with race detector + coverage
make test-integration   # testcontainers-go E2E suite (requires Docker)
make lint               # golangci-lint v2
make fmt                # gofumpt + goimports
make vuln               # govulncheck
make sec                # gosec local
make compose-up         # docker compose up -d (full local stack)
make helm-lint          # helm lint + template
make kustomize-build    # kustomize overlays build
```

## Layout map

| Path                                    | Why it exists                                         |
|-----------------------------------------|--------------------------------------------------------|
| `cmd/proxy/`                            | tiny binary entrypoint                                 |
| `internal/app/`                         | composition root (the only place wiring happens)       |
| `internal/config/`                      | koanf-based config; YAML + env merge                   |
| `internal/engine/`                      | Engine interface + Registry                            |
| `internal/engine/{docling,tika,kreuzberg}/` | adapters                                          |
| `internal/engine/enginetest/`           | shared contract test for all adapters                  |
| `internal/httpclient/`                  | per-backend tuned `*http.Client`                       |
| `internal/breaker/`                     | sony/gobreaker wrapper                                 |
| `internal/tasks/`                       | asynq + Redis async task system                        |
| `internal/observability/`               | zerolog + Prometheus + OTel                            |
| `internal/transport/http/`              | chi router, server, middleware, handlers               |
| `internal/transport/http/middleware/`   | request-id, recover, logging, apikey, rate-limit, etc. |
| `internal/transport/http/handlers/`     | facade convert handlers, async, passthrough, health    |
| `internal/transport/http/reverseproxy/` | native passthrough builder                             |
| `deployments/docker/`                   | Dockerfile + docker-compose                            |
| `deployments/helm/owui-cee-proxy/`      | Helm chart                                             |
| `deployments/kubernetes/`               | base + overlays (ingress-nginx, gateway-api-envoy)     |
| `deployments/systemd/`                  | hardened service unit + idempotent installer          |
| `test/integration/`                     | testcontainers-go E2E suite (`-tags=integration`)      |

## Adding a new engine (recipe)

1. `mkdir internal/engine/<name>/`
2. Implement `Adapter` with `New(cfg, client, breaker)`, `Name()`,
   `Convert(ctx, req)`, `Health(ctx)`. Translate request to backend's
   native API; transform response into Docling envelope shape using
   `wrapDoclingResponse(...)`.
3. Add `<name>_test.go` using `httptest.NewServer` to fake the backend.
   Wire `enginetest.RunContractTests`.
4. Append `<name>` to `EnginesConfig` and add the matching block to
   `configs/config.example.yaml`.
5. Branch in `app.buildRegistry` to construct the adapter.
6. Add an integration test under `test/integration/<name>_e2e_test.go`.
7. (Optional) update `apiKeyHeaderFor` if the engine uses `Authorization`
   instead of `X-Api-Key`.

The slash command `/new-engine <name>` automates steps 1–6 (see
`.claude/commands/new-engine.md`).

## Common pitfalls

- **Don't put secrets in `values.yaml`.** Use the `secrets.existingSecretName`
  knob and supply via SealedSecrets/ExternalSecrets.
- **Don't add fields to `Payload` without `json:"-"`** if they hold
  in-memory buffers. They will get serialised onto Redis otherwise and
  blow up the queue.
- **Don't read `r.Body` twice in handlers.** The bodylimit + multipart
  pipeline is single-pass.
- **Don't forget the contract test.** New adapters that bypass it can
  silently break the registry contract.
- **Don't call `os.Getenv` outside `internal/config`.** All env access
  flows through the config struct so CI/tests can override deterministically.

## Performance budget

- p99 latency overhead introduced by the proxy < 50ms on small files
  (verified with k6 smoke profile in `scripts/load-test.sh`).
- 0 goroutine leaks under soak (60 min).
- Memory ceiling 512 MiB under 100 concurrent requests of 10 MiB
  bodies (configurable but shouldn't drift).
