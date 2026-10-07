// Package integration holds the tests that need real Postgres, Redis,
// MinIO, ClickHouse or stellar-rpc. Every file carries `//go:build
// integration`, so plain `go test ./...` skips it; run `make
// test-integration`.
//
// Most tests start their own testcontainers-go container. The ClickHouse
// suite instead shares one lazily started container per test binary
// (chOnce in clickhouse_harness_test.go, torn down in TestMain) and isolates
// tests by unique contract_id, tx_hash or ledger range. CI only compiles
// the suite (`make test-integration-build`), so an interface change cannot
// break it unnoticed; the Docker run is local or operator-invoked.
package integration
