---
description: Scaffold a new content extraction engine — either an instance of an existing compat_type (config-only) or a brand-new compat_type (adapter package).
argument-hint: <compat_type> <engine_name>
---

Scaffold a new content extraction engine for the OpenWebUI CEE Proxy.

`$1` is the **compat_type** — one of `docling`, `external`,
`docling-external`, `tika`, OR a brand-new compat_type identifier
(e.g., `marker`, `mineru`, `paddle`).

`$2` is the **engine name** the user picks. It must match
`^[a-z0-9][a-z0-9-]{0,31}$` and is used as the YAML map key, the
URL path segment for `/<name>/*`, and the `engine` log label.

You MUST:

1. Read `CLAUDE.md`, `AGENTS.md`, and `docs/ARCHITECTURE.md` first.
2. Decide which path applies:
   - **`$1` is one of the existing four compat_types** → no code
     change. Add an entry to `configs/config.example.yaml` under
     `engines.$2:` with `compat_type: $1`, `enable: true`, an
     appropriate `url`, `api_key_env: "${2^^}_API_KEY"` (uppercased
     with hyphens → underscores), and any relevant `mime_types`.
     Add the matching block to
     `deployments/helm/owui-cee-proxy/values.yaml` under
     `config.engines.$2`. Done — restart picks it up.
   - **`$1` is a NEW compat_type** → continue with steps 3–11 to
     ship a new adapter package.
3. Mirror the structure of `internal/engine/compat/tika/` (the
   closest transformation-heavy adapter):
   - `internal/engine/compat/$1/$1.go` — `Adapter` struct,
     `New(name, cfg, client, br)`, `Name()`, `Capabilities()`,
     `Convert(ctx, req)`, `Health(ctx)`.
   - `internal/engine/compat/$1/transform.go` — request shaping +
     response shaping for both facades you intend to support.
   - `internal/engine/compat/$1/$1_test.go` — table-driven tests
     using `httptest.NewServer` to fake the backend; wire
     `github.com/ctolon/owui-cee-proxy/internal/engine/enginetest.RunContractTests`.
4. Add `Compat<Title> = "$1"` to the constant block in
   `internal/config/config.go` and extend the `compat_type` enum
   validator.
5. Add one switch case to
   `internal/app/app.go::newAdapterByCompat` that constructs the
   adapter when `ec.CompatType == config.Compat<Title>`.
6. Add a `$2` block to `configs/config.example.yaml` under
   `engines:` with `compat_type: $1`. Add the same block to
   `deployments/helm/owui-cee-proxy/values.yaml` under
   `config.engines.$2` and to
   `deployments/kubernetes/base/configmap.yaml`.
7. Add an integration test stub under
   `test/integration/$1_e2e_test.go`.
8. Update the **compat_type matrix** in
   `docs/ARCHITECTURE.md` §4 to list the new compat_type, what its
   backend speaks, and which facades it answers.
9. Update the matrix in `CLAUDE.md` "What this is" similarly.
10. Run `make test` and `make lint`. Both must pass.
11. Run `go vet ./...`.

Do NOT:

- Skip the contract test (`enginetest.RunContractTests`).
- Mock the engine in unit tests using anything other than
  `httptest.NewServer`.
- Hard-code the engine's name anywhere outside its own package.
  Use `Capabilities()` everywhere else.
- Add a YAML field for the raw API-key value — only the env-var
  name (`api_key_env`) goes in YAML.
- Mount a custom passthrough — generic `/<engine-name>/*` is wired
  by `internal/transport/http/routes.go` automatically.

When done, summarise what you added in 5 bullets and ask the user
to review the integration test before running it.
