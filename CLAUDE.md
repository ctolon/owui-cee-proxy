# CLAUDE.md — Project Guide

> Read this first. It encodes the load-bearing decisions that aren't
> obvious from code.

> Companion docs:
> - [`docs/ARCHITECTURE.md`](./docs/ARCHITECTURE.md) — system architecture, request lifecycle, decision log
> - [`docs/BRANCHING.md`](./docs/BRANCHING.md) — `dev` ↔ `main` flow, semver, release contract
> - [`docs/REVIEW.md`](./docs/REVIEW.md) — security/perf review, prioritised follow-ups
> - [`docs/MIGRATION-v0.2.0.md`](./docs/MIGRATION-v0.2.0.md) — v0.1.x → v0.2.0 config migration

## What this is

A Go 1.26.3 reverse proxy that sits between OpenWebUI and any number
of user-defined content extraction backends. Engines are configured
as a **map** in YAML; each entry declares a `compat_type` that
selects the adapter implementation:

| compat_type        | Backend speaks                                       | Reachable via facade(s)        |
|--------------------|------------------------------------------------------|---------------------------------|
| `docling`          | Docling Serve `/v1/convert/{file,source}`            | docling                         |
| `external`         | OpenWebUI `PUT /process` raw + `page_content`        | external                        |
| `docling-external` | both endpoints (separate paths) on same backend      | docling **and** external        |
| `tika`             | Tika `PUT /tika/text` raw + proprietary JSON         | docling **and** external (proxy adapts) |

Kreuzberg is a `compat_type: docling` engine — its `/v1/convert/file`
already speaks the Docling-compat shape.

The proxy exposes:

1. **Docling facade** at `/v1/convert/{file,source,…}` — selects an
   engine via `Registry.Pick(FacadeDocling, mime)`.
2. **External facade** at `PUT /process` — the OpenWebUI external
   loader contract. Selects an engine via `Registry.Pick(FacadeExternal, mime)`.
3. **Native passthrough mounts** at `/<engine-name>/*` for every
   enabled engine — pure streaming reverse proxy onto the backend.
4. **Async siblings** at `/v1/convert/{file,source}/async` with
   `/v1/status/poll/{id}` and `/v1/result/{id}` (Redis-backed via
   asynq).
5. **Operational endpoints**: `/healthz`, `/readyz`, `/metrics` (Prom),
   `/version`.

Engine selection within a facade is **strategy-driven**. The
`routing.strategy` knob selects how the registry picks an engine from
the first file's MIME + filename. Four values:

| Strategy              | Match order                                |
|-----------------------|--------------------------------------------|
| `mime`                | MIME allowlist only                        |
| `extension`           | filename-extension allowlist only          |
| `mime_then_extension` | MIME first; extension fallback (DEFAULT)   |
| `extension_then_mime` | extension first; MIME fallback             |

Each non-default engine declares optional allowlists:

- `engines.<name>.mime_types` — exact matches and `type/*` wildcards
- `engines.<name>.extensions` — case-insensitive, leading dot
  optional (`".pdf"` / `"pdf"` / `".PDF"` canonicalise to `.pdf`)

The first match wins. `routing.default_engine` is the catch-all when
no list matches. Empty `routing.strategy` resolves to
`mime_then_extension`, so v0.4.x configs preserve their MIME-only
behaviour without change.

The proxy also runs a **MIME resolution stage** in front of dispatch.
It sniffs magic bytes when the client's declared Content-Type is
generic, and falls back to the filename extension for Compound File
Binary containers (`.msg`, `.doc`, `.xls`, `.ppt` all share the
`D0CF11E0` header that `mimetype.Detect` cannot disambiguate from
sniffed bytes alone). The built-in 10-entry extension→MIME map is
extensible via `mimedetect.extension_overrides` in YAML.

## Load-bearing invariants — DO NOT VIOLATE

1. **Stateless**. The proxy itself stores nothing in process memory
   beyond config and connection pools. Async tasks live in Redis;
   any replica can serve any task.
2. **Streaming, with a bounded inbound spool**. Outbound to engines
   is `io.Pipe` + streaming multipart. Inbound multipart parts go
   through `handlers/spool.go` — small parts in memory, large parts
   to `O_TMPFILE`. Reason: `multipart.Reader` drains the previous
   part the moment `NextPart()` is called, so each part must be
   fully read before requesting the next; doing otherwise sends
   empty bodies to the engines (silent data loss).
3. **Secrets never live in YAML**. The YAML names the env var
   (`api_key_env`); `config.resolveSecrets` looks it up at startup.
4. **Engine pluggability via compat_type**. Adding a new compat_type
   MUST require zero changes to handlers, routes, middleware, or the
   composition root beyond a single switch case. Touch only:
   - new package under `internal/engine/compat/<compat_type>/`
   - one constant + enum entry in `internal/config/config.go`
   - one switch case in `internal/app/app.go::newAdapterByCompat`
5. **Engine pluggability via config**. Adding another instance of an
   existing compat_type requires **zero code changes** — just add a
   new entry to `engines:` in YAML and reference it from
   `routing.default_engine` if appropriate.
6. **Capabilities, not name checks**. Code that varies behaviour by
   engine MUST query `eng.Capabilities()` (e.g., `HTTPSources`),
   never `eng.Name() == "docling"`. The compat_type system is
   user-defined; engine names are user-controlled.
7. **Liskov via contract test**. Every adapter is verified by
   `internal/engine/enginetest.RunContractTests`, which now also
   asserts `Capabilities().Facades` is non-empty. New adapters MUST
   wire that suite into their `*_test.go`.
8. **Logs always carry `engine`, `engine_url`, `filename`** for
   convert-path requests. Operators rely on these to attribute a
   particular file upload to a particular backend.
9. **Shutdown order**: worker → orchestrator close → trace flush →
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

## Pre-commit invariants — DO NOT SKIP

Before **every** `git add` / `git commit` (including amends, fixups,
and squashes), run the local verification suite. Treat this as a
hard gate — a clean diff is worthless if it doesn't compile, fails
race-detected tests, or trips the linter.

```sh
go test -race -count=1 ./...                            # unit + race
go vet -tags=integration ./test/integration/...         # integration vet (compile-only when Docker unavailable)
golangci-lint run --timeout 5m                          # lint (style + bugs + sec)
```

Optional but recommended when changing deployment artefacts:
`make helm-lint && make kustomize-build`.

Rules:

1. **No commit may land while any of the three checks above is red.**
   Fixing CI after-the-fact is not an acceptable workflow — it
   wastes a runner slot, confuses bisect, and pollutes the
   `chore(release):`-driven changelog. Reproduce the failure locally
   first.
2. **Never bypass with `--no-verify` or `--no-gpg-sign`** unless the
   user explicitly asked. A hook failure means a real check failed;
   investigate and fix the underlying issue.
3. **Don't amend a previous commit when a pre-commit hook fails.**
   The commit didn't happen; `--amend` would rewrite the OLDER
   commit. Re-stage and create a NEW commit.
4. **When working on an in-flight branch, run the relevant subset
   while iterating, then the full sweep before the final commit.**
   Iteration: `go test -race ./internal/<package>/`. Final:
   the three-command block above.
5. **Capture the verification results in the commit message body**
   when the change is substantial (multi-file feature, schema
   migration, security-relevant fix). One line is enough:
   `Verified: go test ./... race-clean, golangci-lint 0 issues,
   integration vet clean.`

These rules are load-bearing: future code that bypasses them often
re-introduces the bug the rule was added to prevent.

## Layout map

| Path                                              | Why it exists                                          |
|---------------------------------------------------|---------------------------------------------------------|
| `cmd/proxy/`                                      | tiny binary entrypoint                                  |
| `internal/app/`                                   | composition root (the only place wiring happens)        |
| `internal/config/`                                | koanf-based config; YAML + env merge                    |
| `internal/engine/`                                | Engine interface + Registry + Capabilities              |
| `internal/engine/compat/<compat_type>/`           | per-compat adapter packages                             |
| `internal/engine/enginetest/`                     | shared contract test for all adapters                   |
| `internal/httpclient/`                            | per-backend tuned `*http.Client`                        |
| `internal/breaker/`                               | sony/gobreaker wrapper                                  |
| `internal/tasks/`                                 | asynq + Redis async task system                         |
| `internal/observability/`                         | zerolog + Prometheus + OTel                             |
| `internal/transport/http/`                        | chi router, server, middleware, handlers                |
| `internal/transport/http/middleware/`             | request-id, recover, logging, apikey, rate-limit, etc.  |
| `internal/transport/http/handlers/`               | facade convert handlers, async, passthrough, health     |
| `internal/transport/http/reverseproxy/`           | native passthrough builder                              |
| `scripts/migrate-config/`                         | v0.1.x → v0.2.0 YAML migration tool                     |
| `deployments/docker/`                             | Dockerfile + docker-compose                             |
| `deployments/helm/owui-cee-proxy/`                | Helm chart                                              |
| `deployments/kubernetes/`                         | base + overlays (ingress-nginx, gateway-api-envoy)      |
| `deployments/systemd/`                            | hardened service unit + idempotent installer           |
| `test/integration/`                               | testcontainers-go E2E suite (`-tags=integration`)       |

## Adding a new engine instance (no code, config-only)

For another instance of an existing compat_type — e.g., a backup
Docling Serve, or a new external loader:

1. Add a new entry to `engines:` in your YAML, e.g.:
   ```yaml
   engines:
     backup-docling:
       compat_type: docling
       enable: true
       url: http://docling-backup:5001
       api_key_env: DOCLING_BACKUP_API_KEY
       mime_types: ["application/pdf"]
   ```
2. (Optional) point `routing.default_engine` at it.
3. Restart. Done — `/backup-docling/*` mounts automatically.

## Adding a new compat_type (recipe — code change)

When you have a backend whose wire format isn't covered by the
existing four compat types (e.g., Marker, MinerU-direct, Paddle):

1. `mkdir internal/engine/compat/<compat_type>/`
2. Implement `Adapter` with `New(name, cfg, client, breaker)`,
   `Name()`, `Capabilities()`, `Convert(ctx, req)`, `Health(ctx)`.
   Use `req.Facade` to shape the response if your adapter answers
   multiple facades.
3. Wire `enginetest.RunContractTests` from your `*_test.go`.
4. Add `Compat<Name> = "<compat_type>"` constant in
   `internal/config/config.go`; extend the compat_type validator
   enum.
5. Add one switch case in `internal/app/app.go::newAdapterByCompat`.
6. Update the matrix in `docs/ARCHITECTURE.md` §4.
7. Optional integration test under `test/integration/`.

The slash command `/new-engine <compat_type> <name>` automates
steps 1–4 (see `.claude/commands/new-engine.md`).

## Common pitfalls

- **Don't put secrets in `values.yaml`.** Use the
  `secrets.existingSecretName` knob and supply via
  SealedSecrets/ExternalSecrets.
- **Don't add fields to `Payload` without `json:"-"`** if they hold
  in-memory buffers. They will get serialised onto Redis otherwise
  and blow up the queue.
- **Don't read `r.Body` twice in handlers.** The bodylimit +
  multipart pipeline is single-pass.
- **Don't gate on engine name.** Use `Capabilities()` —
  `HTTPSources`, `Facades` — instead.
- **Don't forget the contract test.** New adapters that bypass it
  can silently break the registry contract.
- **Don't call `os.Getenv` outside `internal/config`.** All env
  access flows through the config struct so CI/tests can override
  deterministically.

## Performance budget

- p99 latency overhead introduced by the proxy < 50ms on small files
  (verified with k6 smoke profile in `scripts/load-test.sh`).
- 0 goroutine leaks under soak (60 min).
- Memory ceiling 512 MiB under 100 concurrent requests of 10 MiB
  bodies (configurable but shouldn't drift).
