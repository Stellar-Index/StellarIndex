//go:build integration

// Package harness starts the throwaway containers the integration
// suites share.
//
// It is its own package because the Timescale bootstrap has to be
// identical for every caller, and the callers live in packages that
// cannot see each other's test files: the suite under test/integration
// (package integration_test) and the ops-tool tests under scripts/ops
// (package main). A copied bootstrap is a second place for the image
// pin, the container flags or the readiness gate to drift — and the
// readiness gate drifting is not theoretical (see
// TimescaleWaitStrategy).
package harness

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// TimescaleImage pins the container to the TimescaleDB r1 actually
// runs (2.26.x). The old 2.17.2 pin was nine minor versions behind
// prod and BLOCKED the PK-swap-after-decompress that migrations
// 0053–0060 do on compressed hypertables (error 0A000) — so the
// migration round-trip was red AND validated the migrations against a
// TimescaleDB we don't deploy. (audit-2026-06-15, migration dry-run /
// test-vs-prod version drift.)
const TimescaleImage = "timescale/timescaledb:2.26.4-pg15"

// timescaleStartupTimeout is the budget for EACH readiness check
// below. It sits above the library's 60s default because the two
// checks run one after the other against a single boot, and on a
// loaded Docker Desktop running several test shards the entrypoint's
// initdb plus its restart routinely outruns a minute.
const timescaleStartupTimeout = 120 * time.Second

// TimescaleWaitStrategy is the readiness gate for every Timescale
// container the tests start.
//
// The port check is load-bearing, not belt-and-braces. Readiness by
// LOG says nothing about the container's published port bindings, and
// ConnectionString resolves the host port out of exactly those:
// MappedPort walks NetworkSettings.Ports and reports
// `port "5432/tcp" not found` for as long as the binding list is
// empty. A log-only gate therefore hands back a container whose DSN
// cannot be built yet — a prepush run failed with `conn str: port
// "5432/tcp" not found` under parallel shards on Docker Desktop and
// passed on the rerun. ForListeningPort polls the binding into
// existence and then dials it, so by the time the strategy returns
// the DSN is resolvable. Same shape as the ClickHouse harness.
//
// The log check stays, with its occurrence of 2: the postgres image's
// entrypoint runs a local-only bootstrap server — which logs the line
// once — before restarting for real, so the first occurrence is not a
// database anything can connect to.
func TimescaleWaitStrategy() wait.Strategy {
	return wait.ForAll(
		wait.ForListeningPort("5432/tcp"),
		wait.ForLog("database system is ready to accept connections").WithOccurrence(2),
	).WithStartupTimeoutDefault(timescaleStartupTimeout)
}

// StartTimescale boots a throwaway TimescaleDB, enables the extension
// the dev stack's init script enables, and returns the connection DSN.
// Each caller gets its OWN container — no shared fixture — terminated
// on test cleanup.
func StartTimescale(t *testing.T, ctx context.Context) string {
	t.Helper()
	pg, err := tcpostgres.Run(ctx,
		TimescaleImage,
		tcpostgres.WithDatabase("stellarindex"),
		tcpostgres.WithUsername("stellarindex"),
		tcpostgres.WithPassword("stellarindex-test"),
		// ZERO background workers (2026-08-13). The migration chain
		// DROPs the very hypertables whose compression / CAGG-refresh
		// policies it has just created, and TimescaleDB's job scheduler
		// runs those policies concurrently — so a DROP's
		// AccessExclusiveLock and a running job can form a lock CYCLE.
		// CI hit exactly that, twice: "migrate down: deadlock detected,
		// Process 94 waits for AccessExclusiveLock on relation 21724;
		// blocked by process 161" in the round-trip test, then the same
		// deadlock in 0126's DROP MATERIALIZED VIEW through the
		// storage-suite bootstrap. It reproduces only under load, which
		// is why it passes locally in 5s.
		//
		// Retrying is not the fix: a failed migration leaves
		// golang-migrate's version DIRTY, so the retry needs a force.
		// Removing the concurrent actor removes the whole class.
		//
		// This does not weaken anything. Tests assert policies are
		// ATTACHED — a metadata row, still written with no workers to
		// run them — not that they execute; and tests that need CAGG
		// data materialize it by hand.
		//
		// APPEND to the module's own Cmd ("postgres -c fsync=off") —
		// replacing it wholesale makes the container exit 1.
		testcontainers.WithCmdArgs("-c", "timescaledb.max_background_workers=0"),
		testcontainers.WithWaitStrategy(TimescaleWaitStrategy()),
	)
	if err != nil {
		t.Fatalf("start timescale: %v", err)
	}
	// The cleanup runs after the test's context is cancelled, so it needs
	// a context of its own to terminate the container.
	t.Cleanup(func() { _ = pg.Terminate(context.Background()) }) //nolint:contextcheck // cleanup outlives the test context

	dsn, err := pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("conn str: %v", err)
	}
	// Pre-enable the extension (the dev stack does this via
	// deploy/docker-compose/init/00-timescale-extension.sql).
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(ctx, "CREATE EXTENSION IF NOT EXISTS timescaledb"); err != nil {
		t.Fatalf("create extension: %v", err)
	}
	// Prove the zero-background-workers setting took effect: a Cmd
	// override that silently fails to apply looks exactly like a fix
	// while leaving the migrate deadlock race live.
	var workers string
	if err := db.QueryRowContext(ctx, "SHOW timescaledb.max_background_workers").Scan(&workers); err != nil {
		t.Fatalf("read timescaledb.max_background_workers: %v", err)
	}
	if workers != "0" {
		t.Fatalf("timescaledb.max_background_workers = %q, want \"0\" — the container Cmd override did not apply, "+
			"so policy jobs can still deadlock against the migration chain's DROPs", workers)
	}
	return dsn
}
