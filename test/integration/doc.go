// Package integration hosts tests that require real external
// dependencies (Postgres, Redis, MinIO, ClickHouse, stellar-rpc, …)
// rather than mocks. Every file in this directory is guarded by a
// `//go:build integration` build tag; the default `go test ./...` run
// skips it.
//
// Running integration tests:
//
//	make test-integration
//	# or directly:
//	go test -tags=integration -timeout 10m ./test/integration/...
//
// Most tests use `testcontainers-go` to spin up an ephemeral container
// per test (the Postgres/Timescale suite), so no global fixture setup is
// needed. The ClickHouse raw-lake suite (clickhouse_*_test.go) is the one
// exception: ClickHouse boot + schema-apply is ~15-30s, so the whole test
// binary shares ONE ClickHouse container — started lazily on first use
// (chOnce in clickhouse_harness_test.go) and torn down once in TestMain.
// Those tests stay isolated by using unique keys (contract_id / tx_hash /
// ledger range) per test rather than a container each.
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
