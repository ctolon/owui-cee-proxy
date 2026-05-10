// Package integration contains end-to-end tests that spin up real
// Docling Serve, Tika, Kreuzberg and Redis containers via
// testcontainers-go. They are gated by the build tag "integration"
// and require a working Docker daemon. Run with `make test-integration`.
//
//go:build integration

package integration
