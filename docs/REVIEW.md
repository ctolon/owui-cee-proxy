# Project review — known issues, risks, and follow-ups

A consolidated audit of the owui-cee-proxy codebase.
Three audit waves to date:

- **v0.0.1 → v0.0.2**: original two-reviewer pass (security/correctness +
  performance/maintainability). Findings B1–B3, H1–H9, M1–M14, L1–L10,
  N1–N7. Most landed in v0.0.2; status column below tracks each.
- **v0.0.2 → v0.1.x**: branch model + semver release pipeline. No new
  defect findings; release engineering only.
- **v0.1.x → v0.2.0**: user-defined engines + multi-compat-type
  architecture. New surface area, new findings (V1–V5 below).

Severity guide:

- **blocker** — would gate a 1.0 release; security or correctness
  defect with realistic exploit / data-loss path
- **high** — should be fixed before broad production rollout
- **medium** — fix during normal hardening work
- **low** — improves robustness; no urgency
- **nit** — cosmetic, future-tidy

Status column legend:

- ✅ **resolved (vX)** — landed in the named release
- ⚠️ **partial** — addressed but follow-up work tracked
- 🆕 **open** — not yet addressed
- ➖ **deferred** — explicitly punted, see notes

---

## v0.0.1 audit findings

### Blocker

| ID | Issue                                                | Status                |
|----|------------------------------------------------------|------------------------|
| B1 | SSRF DNS-rebinding TOCTOU on `/v1/convert/source`    | ✅ resolved (v0.0.2)   |
| B2 | SSRF block-list incomplete                            | ✅ resolved (v0.0.2)   |
| B3 | Async result fetchable by anyone with task ID         | ✅ resolved (v0.0.2)   |

### High

| ID | Issue                                                | Status                |
|----|------------------------------------------------------|------------------------|
| H1 | Multipart parts buffered to memory                    | ✅ resolved (v0.0.2) — `handlers/spool.go` writes parts > 8 MiB to `O_TMPFILE` |
| H2 | Async path duplicates blob in memory + Redis          | ✅ resolved (v0.0.2) — `tasks.max_blob_bytes` + spool reuse |
| H3 | Reverse-proxy ignores `security.trusted_proxies`      | ✅ resolved (v0.0.2)   |
| H4 | Async response Content-Type replayed verbatim         | ✅ resolved (v0.0.2) — `nosniff` + content-type allowlist |
| H5 | Filename header injection in outbound multipart       | ✅ resolved (v0.0.2) — `safeMultipartFilename` rejects CR/LF/NUL |
| H6 | Prometheus `route` label uses raw URL path            | ✅ resolved (v0.0.2)   |
| H7 | Async task execution timeout = retention              | ✅ resolved (v0.0.2) — `tasks.task_timeout` |
| H8 | `mTLS = require` accepts any client cert              | ✅ resolved (v0.0.2)   |
| H9 | Caller-controlled `X-Request-ID` propagated upstream  | ✅ resolved (v0.0.2)   |

### Medium

| ID  | Issue                                              | Status                |
|-----|----------------------------------------------------|------------------------|
| M1  | `Timeout` middleware not wired                     | ✅ resolved (v0.0.2)   |
| M2  | `WriteTimeout` defaults to 0                       | ✅ resolved (v0.0.2)   |
| M3  | Reverse-proxy path traversal — `path.Clean`        | ✅ resolved (v0.0.2)   |
| M4  | Health endpoint echoes upstream errors             | ✅ resolved (v0.0.2)   |
| M5  | Convert handler reflects engine error verbatim     | ✅ resolved (v0.0.2)   |
| M6  | Status lookup scans every queue                    | ✅ resolved (v0.0.2)   |
| M7  | `ResponseHeaderTimeout` 30s caps long extractions  | ✅ resolved (v0.0.2)   |
| M8  | `responseWriter` lacks Hijacker / atomic `wrote`   | ✅ resolved (v0.0.2)   |
| M9  | Slice env-override undefined                       | ✅ resolved (v0.0.2) — comma-split for `[]string` keys |
| M10 | Body-size cap registered after access log/metrics  | ✅ resolved (v0.0.2)   |
| M11 | `zerolog.AddCaller` enabled by default             | ✅ resolved (v0.0.2)   |
| M12 | `recover` ordering — `RequestID` is outside it     | ✅ resolved (v0.0.2)   |
| M13 | Source mode silently uses default engine           | ✅ resolved (v0.0.2)   |
| M14 | Async submit enqueues even when source unsupported | ✅ resolved (v0.2.0) — capability-based: `eng.Capabilities().HTTPSources` (was hard-coded `eng.Name() != engine.Docling`) |

### Low

| ID  | Issue                                              | Status                |
|-----|----------------------------------------------------|------------------------|
| L1  | Constant-time API-key compare leaks length         | ✅ resolved (v0.0.2) — SHA-256 both sides |
| L2  | Validator only catches missing URL on default      | ✅ resolved (v0.0.2)   |
| L3  | `isExternalURL` package-global mutable             | ✅ resolved (v0.0.2)   |
| L4  | `panic(err)` on passthrough mount build failure    | ✅ resolved (v0.0.2)   |
| L5  | Worker queue weights hard-coded                    | ✅ resolved (v0.0.2)   |
| L6  | `Tika.convertOne` uses `io.ReadAll`                | ✅ resolved (v0.0.2)   |
| L7  | `Health.Readiness` polls engines sequentially      | ✅ resolved (v0.0.2)   |
| L8  | `_ = json.Marshal` placeholder lines               | ✅ resolved (v0.0.2)   |
| L9  | `BodyBytes` histogram only records `out`           | ✅ resolved (v0.0.2)   |
| L10 | Redis URL credentials visible in `Config` printout | ✅ resolved (v0.0.2) — `Secret` type with redacted `String()` |

### Nits

| ID  | Issue                                              | Status                |
|-----|----------------------------------------------------|------------------------|
| N1  | `strconvItoa` reinvented in docling.go             | ✅ resolved (v0.0.2)   |
| N2  | `applyForwardHeaders` allowlist slice scan         | ✅ resolved (v0.0.2)   |
| N3  | `noop` cleanup function dead                       | ✅ resolved (v0.0.2)   |
| N4  | `Engine.Pick` keeps default in `order` then skips  | ✅ resolved (v0.0.2)   |
| N5  | asynq error handler logs without `task_id`         | 🆕 open — small, deferred to a follow-up patch |
| N6  | `cleanup` callback dead in convert handler         | ✅ resolved (v0.0.2)   |
| N7  | `_ctxValue` helper unused                          | ✅ resolved (v0.0.2)   |

---

## v0.2.0 audit findings (new surface area)

The user-defined engine map and the new external facade widen the
attack and operability surface. The findings below were authored
during the v0.2.0 design pass; treat as the next pre-1.0 sweep.

### V1 · Engine name as URL path segment — charset must be tight

- **Severity**: high
- **Where**: `internal/config/config.go` (`engineNameRE`),
  `internal/transport/http/routes.go` (passthrough mount loop)
- **Issue**: Each engine in `engines.<name>` is mounted at
  `/<name>/*` for the passthrough proxy. If the name validator allowed
  `/`, `..`, `?`, `#`, or non-ASCII bytes, the user could (deliberately
  or by typo) shadow `/v1/convert/file` or escape to a different mount.
- **Mitigation in v0.2.0**: regex `^[a-z0-9][a-z0-9-]{0,31}$`. No
  uppercase, no underscore, no leading hyphen. Length cap 32 chars.
- **Follow-up**: revisit if users request mixed-case (URL paths are
  case-insensitive on some proxies but not on chi); add unit test
  asserting all forbidden chars 400 at config load.

### V2 · `auth_header` is user-controlled — header injection on outbound

- **Severity**: high
- **Where**: `internal/config/config.go` (`authHeaderRE`),
  `internal/engine/compat/*/`'s `ApplyAuth`
- **Issue**: Each engine declares its own `auth_header` (default
  `X-Api-Key`). This string is set on outbound requests via
  `req.Header.Set(cfg.AuthHeader, value)`. A malicious or careless
  config containing CR/LF in `auth_header` would inject arbitrary
  request headers into upstream traffic.
- **Mitigation in v0.2.0**: regex `^[A-Za-z][A-Za-z0-9-]{0,63}$`.
  Validated at config load. CR/LF/colon impossible.
- **Follow-up**: add an explicit allowlist of header names if any
  operator pushes back on the regex (e.g., wants `X_Api_Key` with
  underscore).

### V3 · External facade `X-Filename` decoded → must be sanitised

- **Severity**: high
- **Where**: `internal/transport/http/handlers/external.go` (`Process`)
- **Issue**: OpenWebUI's external loader contract sends the original
  filename in `X-Filename`, URL-decoded once at the proxy boundary.
  Logging or forwarding this value without scrubbing CR/LF/NUL would
  let attackers inject log lines or smuggle multipart parts when the
  proxy adapts the request to a docling-shape engine.
- **Mitigation in v0.2.0**: handler rejects `X-Filename` with control
  bytes at the boundary; `safeMultipartFilename` re-runs on adapted
  outbound.
- **Follow-up**: add fuzz test `external_test.go::FuzzXFilename`.

### V4 · Composite `docling-external` doubles fault surface

- **Severity**: medium
- **Where**: `internal/engine/compat/doclingexternal/doclingexternal.go`
- **Issue**: The composite holds two child adapters (one per
  endpoint). If only one endpoint is healthy, the composite's
  `Health()` may report `ok` while the other facade is broken.
- **Mitigation in v0.2.0**: `Health()` probes both children; failure
  on either child propagates as unhealthy.
- **Follow-up**: per-facade health labels in `/readyz` so operators
  can see which endpoint is dead.

### V5 · Capability-based dispatch generalises M14 — verify in tests

- **Severity**: low (already in scope)
- **Where**: `internal/transport/http/handlers/async.go`
- **Issue**: M14 was fixed for Docling/Tika/Kreuzberg specifically.
  The new check `!eng.Capabilities().HTTPSources` covers them all,
  but a future engine that declares `HTTPSources: false` and is also
  not the default could regress if test coverage stays anchored on
  the old engine names.
- **Mitigation in v0.2.0**: contract test
  `enginetest/contract.go::capabilities_declares_at_least_one_facade`
  + handler-level table test.
- **Follow-up**: extend table to assert the 400 path for every
  registered compat_type that doesn't support sources.

---

## Roadmap aggregation

| When               | Pickup                                                                |
|--------------------|------------------------------------------------------------------------|
| Pre-1.0 hardening  | V1, V2, V3 (already mitigated; tests + docs to follow)                |
| Post-1.0           | V4, V5 — observability and contract-test polish                        |
| Continuous tidy    | N5 (asynq error handler `task_id`)                                    |

---

## Process notes

The v0.0.1 audit was produced by two parallel reviewers reading the
codebase statically. The v0.2.0 review is design-time (authored
alongside the refactor plan). Findings cite concrete file paths;
line numbers drift as the code evolves and should be re-resolved at
fix time.

To re-run the audit, see the prompts captured under
`test/integration/scripts/`.
