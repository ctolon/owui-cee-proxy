# Contributing

Thanks for your interest! A few ground rules to make reviews smooth.

## Branching

This repo uses a two-branch model — see [`docs/BRANCHING.md`](./docs/BRANCHING.md)
for the full reference.

- **Branch from `dev`** for any feature/fix work
  (`git switch -c feat/my-thing dev`).
- Open the feature PR against `dev`. CI runs on every push to your
  branch and on the PR.
- Periodically a maintainer opens a PR `dev → main`. **Merging that
  PR is the release event** — `release.yaml` builds and publishes
  the artifacts.
- `main` accepts squash-merges only and the squash-merge title is
  the conventional-commit message that drives the next semver bump.

## PR title (the release contract)

Squash-merge of `dev → main` produces one commit; its title must
follow [Conventional Commits](https://www.conventionalcommits.org/):

```
feat(engine): add MinerU adapter
fix(ssrf): close DNS rebind window
feat!: drop deprecated /v0/extract        # major bump (BREAKING CHANGE)
chore: bump golangci                       # no release
```

Allowed types: `feat`, `fix`, `perf`, `refactor`, `docs`, `test`,
`build`, `ci`, `chore`, `revert`. The PR title is enforced by
`pr-checks.yaml` on every PR-to-main; a malformed title blocks merge.

The PR **body** becomes the GitHub Release notes, so write it like
release notes — sections, code snippets, migration guidance.

## Before opening a PR

1. `make fmt` (gofumpt + goimports).
2. `make lint` (golangci-lint v2, strict config).
3. `make test` (race detector + coverage).
4. `make test-integration` if your change touches an engine adapter,
   the transport layer, or the async system.
5. `make helm-lint` and `make kustomize-build` if you touched anything
   under `deployments/`.

## Commit hygiene

- Use imperative mood: "add Tika passthrough", not "added Tika
  passthrough". (The squash-merge title — see above — is the one
  commit message that ships to `main`; intermediate commits on your
  feature branch can be informal.)
- One concern per PR.
- No mixing of refactor + behaviour change in the same PR.

## Adding a new engine

Use the slash command `/new-engine <name>`. It scaffolds the adapter,
config block, registry wiring, contract test, and integration test
stub in one go. See `CLAUDE.md` for the manual recipe.

## Updating dependencies

Run `go get -u ./... && go mod tidy`. Then re-run the full security
pipeline locally:

```sh
make vuln
make sec
```

## Reporting bugs

Use GitHub Issues. Include:

- proxy version (`owui-cee-proxy -version`)
- relevant log lines (with `request_id`)
- minimal reproduction (`docker compose up` snippet preferred)

## Licensing of contributions

By submitting a pull request you agree to license your contribution
under the project's license — Apache License 2.0. See the
[`LICENSE`](./LICENSE) and [`NOTICE`](./NOTICE) files for details.
No CLA is required.
