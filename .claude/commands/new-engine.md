---
description: Scaffold a new content extraction engine adapter end-to-end.
argument-hint: <engine_name>
---

Scaffold a new content extraction engine called `$1` for the
OpenWebUI CEE Proxy.

You MUST:

1. Read `CLAUDE.md` and `AGENTS.md` first.
2. Mirror the structure of `internal/engine/tika/` (the closest
   transformation-heavy adapter):
   - `internal/engine/$1/$1.go` — Adapter struct, `New(cfg, client, br)`,
     `Name()`, `Convert(ctx, req)`, `Health(ctx)`.
   - `internal/engine/$1/transform.go` — request shaping +
     `parseXResponse` + `wrapDoclingResponse`.
   - `internal/engine/$1/$1_test.go` — table-driven tests using
     `httptest.NewServer` to fake the backend; wire
     `github.com/ctolon/owui-cee-proxy/internal/engine/enginetest.RunContractTests`.
3. Add the engine constant to `internal/engine/engine.go`.
4. Add a `$1` block to `internal/config/config.go` (`EnginesConfig`
   struct), `Default()` defaults, and `configs/config.example.yaml`.
5. Branch in `internal/app/app.go` `buildRegistry` to construct the
   adapter when enabled.
6. If the engine uses `Authorization` instead of `X-Api-Key`, branch
   in `internal/transport/http/routes.go` `apiKeyHeaderFor`.
7. Add a passthrough mount in `routes.go` and a `passthrough.$1_prefix`
   field in the routing config.
8. Add an integration test stub under `test/integration/$1_e2e_test.go`.
9. Add the chart `values.yaml` and `configmap.yaml` block under
   `deployments/helm/owui-cee-proxy/values.yaml.config.engines.$1`.
10. Run `make test` and `make lint`. Both must pass.
11. Update `CLAUDE.md` engine table.

Do NOT:

- Skip the contract test.
- Mock the engine in unit tests using anything other than `httptest.NewServer`.
- Add a YAML field for the API key — use `${1}_api_key_env: "${1^^}_API_KEY"`.

When done, summarise what you added in 5 bullets and ask the user to
review the integration test before running it.
