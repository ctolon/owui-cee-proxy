# Security Policy

## Reporting a Vulnerability

If you believe you have found a security vulnerability, please **do not**
open a public GitHub issue. Email the maintainers at
`security@example.invalid` (replace before publishing) with:

- a description of the issue
- the affected version(s)
- steps to reproduce
- the impact you have assessed

We aim to acknowledge reports within 72 hours and to issue a fix or a
mitigation plan within 30 days, depending on severity.

## Threat model summary

The proxy sits between OpenWebUI and one or more content-extraction
backends. Trust boundaries:

| Boundary                          | Posture                                            |
|-----------------------------------|----------------------------------------------------|
| OpenWebUI → proxy                 | Authenticated when `security.require_api_key=true` |
| Proxy → backend (Docling/Tika/Kreuzberg) | Authenticated via env-supplied API key      |
| Operator → proxy (Admin / metrics) | API-key gated when enforcement is on              |
| Proxy → Redis (async)             | Operator-supplied URL, may include password        |
| Source URLs in `/v1/convert/source` | Validated against blocked CIDR list (anti-SSRF) |

Mitigations the project ships with:

- TLS 1.3 default, optional mTLS.
- Multi-key proxy auth with constant-time comparison.
- Body size cap, header count limits.
- SSRF allowlist (default deny private/loopback/metadata IPs).
- Per-backend circuit breakers + per-backend rate limit.
- Distroless image, non-root, read-only rootfs, no capabilities.
- systemd hardening (`ProtectSystem=strict`,
  `MemoryDenyWriteExecute=true`, …).
- NetworkPolicy template restricting egress.
- Manual security pipeline (gosec, govulncheck, trivy, semgrep,
  syft+grype, dockle, checkov).

## Scope

In scope: the proxy binary, its configuration loader, its container
image, its Helm chart, its Kustomize manifests, its systemd unit, and
its CI workflows.

Out of scope: vulnerabilities in upstream Docling Serve, Apache Tika,
Kreuzberg, Redis, or OpenWebUI themselves. Report those upstream.
