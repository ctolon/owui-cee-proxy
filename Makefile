SHELL := /usr/bin/env bash

BINARY      := owui-cee-proxy
PKG         := github.com/ctolon/owui-cee-proxy
CMD         := ./cmd/proxy
BIN_DIR     := bin
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE        ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS     := -s -w \
               -X $(PKG)/internal/version.Version=$(VERSION) \
               -X $(PKG)/internal/version.Commit=$(COMMIT) \
               -X $(PKG)/internal/version.Date=$(DATE)

GO          ?= go
GOFLAGS     ?=
GOOS        ?= $(shell $(GO) env GOOS)
GOARCH      ?= $(shell $(GO) env GOARCH)

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show available targets
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z0-9_.-]+:.*?## / {printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: tidy
tidy: ## go mod tidy
	$(GO) mod tidy

.PHONY: build
build: ## Build the proxy binary
	mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) \
		$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/$(BINARY) $(CMD)

.PHONY: run
run: build ## Build and run with example config
	OWUI_PROXY_CONFIG=configs/config.example.yaml ./$(BIN_DIR)/$(BINARY)

.PHONY: test
test: ## Unit tests with race detector
	$(GO) test -race -count=1 -coverprofile=cov.out ./...

.PHONY: cover
cover: test ## Show coverage HTML
	$(GO) tool cover -html=cov.out

.PHONY: test-integration
test-integration: ## Integration tests (testcontainers); requires Docker
	$(GO) test -race -count=1 -tags=integration -timeout=15m ./test/integration/...

.PHONY: bench
bench: ## Run benchmarks
	$(GO) test -bench=. -benchmem -run=^$$ ./...

.PHONY: lint
lint: ## golangci-lint
	golangci-lint run ./...

.PHONY: fmt
fmt: ## gofumpt + goimports
	gofumpt -w .
	goimports -w -local $(PKG) .

.PHONY: vet
vet: ## go vet
	$(GO) vet ./...

.PHONY: vuln
vuln: ## govulncheck
	govulncheck ./...

.PHONY: sec
sec: ## gosec local scan
	gosec -quiet ./...

.PHONY: docker-build
docker-build: ## Build container image
	docker build -t $(BINARY):$(VERSION) -f deployments/docker/Dockerfile .

.PHONY: compose-up
compose-up: ## Bring up local dev stack
	docker compose -f deployments/docker/docker-compose.yaml up -d --build

.PHONY: compose-down
compose-down:
	docker compose -f deployments/docker/docker-compose.yaml down -v

.PHONY: helm-lint
helm-lint:
	helm lint deployments/helm/owui-cee-proxy
	helm template owui-cee-proxy deployments/helm/owui-cee-proxy >/dev/null

.PHONY: kustomize-build
kustomize-build:
	kustomize build deployments/kubernetes/overlays/ingress-nginx >/dev/null
	kustomize build deployments/kubernetes/overlays/gateway-api-envoy >/dev/null

.PHONY: kubeconform
kubeconform: kustomize-build
	kustomize build deployments/kubernetes/overlays/ingress-nginx | kubeconform -strict -summary
	kustomize build deployments/kubernetes/overlays/gateway-api-envoy | kubeconform -strict -summary

.PHONY: clean
clean:
	rm -rf $(BIN_DIR) dist cov.out coverage.txt
