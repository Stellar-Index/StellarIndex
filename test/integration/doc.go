// Package integration hosts tests that require real external
// dependencies (Postgres, Redis, MinIO, stellar-rpc, …) rather than
// mocks. Every file in this directory is guarded by a `//go:build
// integration` build tag; the default `go test ./...` run skips it.
//
// Running integration tests:
//
//	make test-integration
//	# or directly:
//	go test -tags=integration -timeout 10m ./test/integration/...
//
// Tests use `testcontainers-go` to spin up ephemeral containers per
// test, so no global fixture setup is needed — each test is fully
// self-contained. CI runs the full suite nightly + on the
// `ready-for-integration` label per CONTRIBUTING.md.
//
// See docs/architecture/repo-hygiene-plan.md §9 (testing discipline).
package integration
