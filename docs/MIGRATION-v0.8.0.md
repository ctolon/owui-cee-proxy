# Migration: v0.7.x → v0.8.0

## `engines.<name>.paths` shape change

Pre-v0.8.0 the per-engine path overrides were keyed on a
compat-prefixed struct field:

```yaml
# v0.7.x — REMOVED
engines:
  main-docling:
    compat_type: docling
    paths:
      docling_convert_file:   /api/v2/extract
      docling_convert_source: /api/v2/source

  loader:
    compat_type: external
    paths:
      external_process: /process

  text-extract:
    compat_type: tika
    paths:
      tika_text: /tika/text
```

v0.8.0 replaces the struct with a generic `map[string]string`. The
compat-type prefix on each key was redundant — an engine has
exactly one `compat_type`, so the namespace was always implied.
The new shape uses adapter-owned key names:

```yaml
# v0.8.0+
engines:
  main-docling:
    compat_type: docling
    paths:
      convert_file:   /api/v2/extract
      convert_source: /api/v2/source

  loader:
    compat_type: external
    paths:
      process: /process

  text-extract:
    compat_type: tika
    paths:
      text: /tika/text
```

### Key mapping

| v0.7.x key                | v0.8.0 key       | Compat       |
|---------------------------|------------------|--------------|
| `docling_convert_file`    | `convert_file`   | `docling`    |
| `docling_convert_source`  | `convert_source` | `docling`    |
| `external_process`        | `process`        | `external`   |
| `tika_text`               | `text`           | `tika`       |

### Why

The old struct had an N×M growth surface: every new compat type
added M new fields (one per known path), and every new path key
added N new fields (one per existing compat). The map-shape adds
zero schema cells per axis — adapter packages own their key
constants (`docling.PathConvertFile`, `external.PathProcess`,
`tika.PathText`).

Plugin authors writing out-of-tree adapters can now declare their
own key constants without touching `internal/config/config.go`.

### How to migrate

1. Locate every `engines.*.paths.*` block in your YAML.
2. Drop the compat-type prefix from each key per the table above.
3. Restart the proxy. The Go binary rejects the old keys; you'll
   see an `unknown field` error pointing at the offending line.

If you weren't overriding any paths (the default for most
deployments) no action is required.
