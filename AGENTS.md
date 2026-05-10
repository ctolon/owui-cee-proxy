# AGENTS.md — Charter for Repository Agents

This file documents the Claude subagents that ship with the repository
and the rules they follow. Agents live in `.claude/agents/`. Slash
commands live in `.claude/commands/`. Both are picked up by Claude
Code automatically.

## Available agents

| Agent name           | Purpose                                                             | Default model |
|----------------------|---------------------------------------------------------------------|---------------|
| `go-engineer`        | Implement and modify Go code while honouring CLAUDE.md invariants.  | sonnet        |
| `helm-author`        | Author/modify Helm chart, Kustomize overlays, raw manifests.        | sonnet        |
| `security-reviewer`  | OWASP-lens review of changes; threat-model deltas.                  | opus          |
| `perf-tester`        | k6 / pprof scenarios + interpretation.                              | sonnet        |

Use them via the `Agent` tool with the matching `subagent_type`.

## Rules every agent follows

1. **CLAUDE.md is law**. The "load-bearing invariants" list is
   non-negotiable; deviations need explicit human approval.
2. **No secrets in code, YAML, or commit messages**. Ever.
3. **No new engine without contract test**. `enginetest.RunContractTests`
   must be wired before merge.
4. **No `replace_all` blind renames** that touch generated code,
   third-party vendor trees, or test fixtures.
5. **No `--no-verify` git pushes**. Hooks are there for a reason.
6. **No skipping integration tests**. Removing `-tags=integration`
   coverage to make CI green is a bug, not a fix.
7. **No buffering full request bodies on hot paths**. Use streams
   and `io.Pipe`.
8. **Unauthenticated mutating actions** (push, deploy, kubectl apply,
   helm install on shared cluster) require explicit human confirmation.
9. **PRs and commit messages are concise and free of marketing copy**.
   No "🚀 amazing feature".

## Coordination notes

- For multi-area changes (Go code + Helm + manifests), prefer running
  `go-engineer` and `helm-author` sequentially rather than in parallel.
  The `helm-author` may need to read the freshly-written Go config to
  know which keys to surface in `values.yaml`.
- Always invoke `security-reviewer` before merging changes that touch
  authentication, TLS, request parsing, or external HTTP egress.
- `perf-tester` runs only when the diff plausibly affects throughput;
  small refactors don't need it.

## Slash commands

| Command         | Effect                                                    |
|-----------------|-----------------------------------------------------------|
| `/new-engine`   | Scaffold a new engine adapter end-to-end (config, code, tests). |

See the markdown files in `.claude/commands/` for invocation details.
