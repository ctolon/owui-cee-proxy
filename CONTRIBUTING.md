# Contributing

Thanks for your interest! A few ground rules to make reviews smooth:

## Before opening a PR

1. `make fmt` (gofumpt + goimports).
2. `make lint` (golangci-lint v2, strict config).
3. `make test` (race detector + coverage).
4. `make test-integration` if your change touches an engine adapter,
   the transport layer, or the async system.
5. `make helm-lint` and `make kustomize-build` if you touched anything
   under `deployments/`.

## Commit hygiene

- Use imperative mood: "add Tika passthrough", not "added Tika passthrough".
- One concern per commit. PRs may bundle multiple commits.
- No mixing of refactor + behaviour change in the same commit.

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
