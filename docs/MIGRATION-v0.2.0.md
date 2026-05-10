# Migration to v0.2.0

v0.2.0 introduces user-defined engines and a `compat_type` dispatch.
The config schema for `engines:` and `routing:` changes shape; this
document is a one-screen reference for upgrading.

## What changed at the YAML layer

### routing

| v0.1.x                                    | v0.2.0                                          |
|-------------------------------------------|-------------------------------------------------|
| `routing.default_cee_engine: docling`     | `routing.default_engine: main-docling`          |
| `routing.facade_path_prefix: /v1`         | `routing.facade.docling.prefix: /v1`            |
| (no equivalent)                           | `routing.facade.external.path: /process` (new)  |
| `routing.passthrough.docling_prefix: /docling` | `routing.passthrough.enabled: true` (mounts /\<engine-name\>/* per engine) |
| `routing.passthrough.tika_prefix: /tika`  | (dropped — derived from engine name)            |
| `routing.passthrough.kreuzberg_prefix: /kreuzberg` | (dropped — derived from engine name)   |

### engines

The struct `engines: { docling, tika, kreuzberg }` becomes a
**user-keyed map**:

```yaml
# v0.1.x                          # v0.2.0
engines:                           engines:
  docling:                           main-docling:        # name is user-supplied
    enable: true                       enable: true
    url: ...                           compat_type: docling   # NEW
                                       url: ...
                                       auth_header: X-Api-Key # NEW (was implicit)
                                       auth_scheme: raw       # NEW
```

Engine names must match `^[a-z0-9-]{1,32}$`. They become the URL
path segment for the passthrough mount (`/<engine-name>/*`).

### compat_type values

| compat_type        | Backend speaks                                   | Reachable via            |
|--------------------|---------------------------------------------------|--------------------------|
| `docling`          | Docling Serve `/v1/convert/{file,source}`        | docling facade           |
| `external`         | OpenWebUI `PUT /process` raw, `page_content`     | external facade          |
| `docling-external` | both endpoints (separate paths) on same backend  | docling AND external     |
| `tika`             | Tika `PUT /tika/text` raw, proprietary JSON      | docling AND external (proxy adapts) |

Kreuzberg is **not** a separate compat_type — its `/v1/convert/file`
mirrors Docling, so it's `compat_type: docling`.

### auth

`apiKeyHeaderFor` (which switched on engine name) is gone. Each
engine declares its own `auth_header` (default `X-Api-Key`) and
`auth_scheme` (`raw` | `bearer`). The default is auto-selected from
`compat_type`: `external` engines get `Authorization` + `bearer`,
others get `X-Api-Key` + `raw`.

## Automated migration

Use the bundled tool:

```sh
go run ./scripts/migrate-config configs/old.yaml > configs/new.yaml
diff -u configs/old.yaml configs/new.yaml
```

The migration:

- Renames `default_cee_engine` → `default_engine`.
- Maps `facade_path_prefix` → `facade.docling.prefix`.
- Replaces `passthrough.<n>_prefix` keys with `passthrough.enabled: true`.
- Tags `engines.docling` → `compat_type: docling`, `auth_header: X-Api-Key`.
- Tags `engines.tika` → `compat_type: tika`, `auth_header: X-Api-Key`.
- Tags `engines.kreuzberg` → `compat_type: docling`, `auth_header: Authorization`, `auth_scheme: bearer`.

## Helm + Kubernetes

`deployments/helm/owui-cee-proxy/values.yaml` and
`deployments/kubernetes/base/configmap.yaml` ship the new schema.
GitOps users should diff their override files and run the migration
script on any in-cluster ConfigMap exports.

## OpenWebUI integration changes

v0.2.0 exposes both facades by default. OpenWebUI can now be pointed
at the proxy via either env var:

```
# Option A — docling facade
CONTENT_EXTRACTION_ENGINE=docling
DOCLING_SERVER_URL=http://owui-cee-proxy:8080

# Option B — external facade (NEW in v0.2.0)
CONTENT_EXTRACTION_ENGINE=external
EXTERNAL_DOCUMENT_LOADER_URL=http://owui-cee-proxy:8080
```

Both produce identical extracted text per fixture; the difference is
which inbound protocol OpenWebUI uses to talk to the proxy.

## Out of scope

- `docs/HOT_RELOAD_ROADMAP.md` was deleted in v0.2.0; hot reload is
  no longer on the immediate roadmap. Operators wanting config
  changes should issue a normal pod restart / rolling deploy.
