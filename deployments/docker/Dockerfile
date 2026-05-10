# syntax=docker/dockerfile:1.7

# ─── build stage ──────────────────────────────────────────────────────
FROM --platform=$BUILDPLATFORM golang:1.26.1-alpine AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown

WORKDIR /src

# Cache module downloads.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod download

COPY . .

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
      go build -trimpath \
        -ldflags "-s -w \
          -X github.com/ctolon/owui-cee-proxy/internal/version.Version=${VERSION} \
          -X github.com/ctolon/owui-cee-proxy/internal/version.Commit=${COMMIT} \
          -X github.com/ctolon/owui-cee-proxy/internal/version.Date=${DATE}" \
        -o /out/owui-cee-proxy ./cmd/proxy

# ─── runtime stage ────────────────────────────────────────────────────
FROM gcr.io/distroless/static-debian12:nonroot

# Redeclare in this stage — Dockerfile ARG scopes are per-stage.
ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown

WORKDIR /app

COPY --from=build /out/owui-cee-proxy /app/owui-cee-proxy
COPY configs/config.example.yaml /app/config.yaml

# OCI image labels: GHCR uses image.source to auto-link the package to
# the source repo (public repos → public packages on push).
LABEL org.opencontainers.image.title="owui-cee-proxy" \
      org.opencontainers.image.description="OpenWebUI Content Extraction Engine proxy" \
      org.opencontainers.image.source="https://github.com/ctolon/owui-cee-proxy" \
      org.opencontainers.image.url="https://github.com/ctolon/owui-cee-proxy" \
      org.opencontainers.image.documentation="https://github.com/ctolon/owui-cee-proxy/blob/main/README.md" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.vendor="ctolon" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${DATE}"

USER nonroot:nonroot
EXPOSE 8080

ENV OWUI_PROXY_CONFIG=/app/config.yaml

ENTRYPOINT ["/app/owui-cee-proxy"]
CMD ["-config", "/app/config.yaml"]
