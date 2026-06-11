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
// self-contained.
//
// CI runs `make test-integration-build` (the verify gate compiles every
// integration-tagged package without Docker, so an interface change
// can't silently break the suite — F-1334). The full Docker run
// (`make test-integration`) is operator-/local-invoked; there is no
// scheduled nightly Docker job today (the GitHub Actions spend cap keeps
// heavy scheduled jobs off — see the k6-weekly precedent).
//
// See docs/architecture/repo-hygiene-plan.md §9 (testing discipline).
package integration
