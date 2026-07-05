//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// clickhouseImage pins the ClickHouse server image for the raw-lake (ADR-0034)
// integration suite. A stable LTS line — the schema uses only long-GA features
// (ReplacingMergeTree FINAL, AggregatingMergeTree / uniqExact, Int128,
// bloom_filter skip-indexes, materialized views). Keep it a real tag (not
// `latest`) so a CI run is reproducible.
const clickhouseImage = "clickhouse/clickhouse-server:24.8"

// The shared ClickHouse container. Unlike the per-test Postgres containers
// (storage_test.go), ClickHouse startup + schema apply is ~15-30s, so the whole
// integration binary shares ONE container (started lazily on first use). Tests
// stay isolated by using unique keys (contract_id / tx_hash / ledger range) per
// test rather than a container each — the lake tables are ReplacingMergeTree and
// every read filters by those keys.
//
// Lifecycle: started on demand via clickhouseAddr; torn down once in TestMain
// after the whole suite runs (so it never starts at all when only the Postgres
// integration tests are selected with -run).
var (
	chOnce      sync.Once
	chContainer testcontainers.Container
	chAddr      string
	chErr       error
)

// TestMain owns the shared ClickHouse container's teardown. It does NOT start
// anything eagerly — chOnce fires only when a ClickHouse-backed test calls
// clickhouseAddr — so `go test -run TestStoreRoundTrip` (a Postgres test) never
// pays the ClickHouse startup cost.
func TestMain(m *testing.M) {
	code := m.Run()
	if chContainer != nil {
		_ = chContainer.Terminate(context.Background())
	}
	os.Exit(code)
}

// clickhouseAddr starts the shared container on first call (applying the
// certified Tier-1 schema), then returns its native-protocol `host:port` for
// the repo's own clickhouse constructors (chstore.Open / NewSupplyReader /
// NewExplorerReader …).
func clickhouseAddr(t *testing.T) string {
	t.Helper()
	chOnce.Do(func() {
		chAddr, chContainer, chErr = startClickHouse(context.Background())
	})
	if chErr != nil {
		t.Fatalf("start clickhouse container: %v", chErr)
	}
	return chAddr
}

// startClickHouse boots a ClickHouse container, waits for it to accept
// connections, resolves the mapped native port, and applies
// deploy/clickhouse/tier1_schema.sql (which CREATEs the `stellar` database +
// every lake table/MV). Returns the native `host:port` address.
func startClickHouse(ctx context.Context) (string, testcontainers.Container, error) {
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image: clickhouseImage,
			// 9000 = native protocol (clickhouse-go driver); 8123 = HTTP (used
			// only for the /ping readiness probe).
			ExposedPorts: []string{"9000/tcp", "8123/tcp"},
			// The 24.x image's entrypoint, when no CLICKHOUSE_USER/PASSWORD is
			// set, RESTRICTS the `default` user to localhost (::1/127.0.0.1),
			// so a connection over the mapped port fails with a misleading
			// AUTHENTICATION_FAILED. SKIP_USER_SETUP leaves the stock users.xml
			// (default user, empty password, all networks) intact — which is
			// what the repo's clickhouse constructors expect (they auth as
			// `default` with no password). This is a throwaway test container,
			// so open network access carries no risk.
			Env: map[string]string{"CLICKHOUSE_SKIP_USER_SETUP": "1"},
			WaitingFor: wait.ForAll(
				wait.ForListeningPort("9000/tcp"),
				wait.ForHTTP("/ping").WithPort("8123/tcp").
					WithStatusCodeMatcher(func(status int) bool { return status == 200 }),
			).WithStartupTimeoutDefault(180 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		return "", nil, fmt.Errorf("clickhouse: start container: %w", err)
	}

	host, err := container.Host(ctx)
	if err != nil {
		_ = container.Terminate(ctx)
		return "", nil, fmt.Errorf("clickhouse: container host: %w", err)
	}
	port, err := container.MappedPort(ctx, "9000/tcp")
	if err != nil {
		_ = container.Terminate(ctx)
		return "", nil, fmt.Errorf("clickhouse: mapped native port: %w", err)
	}
	addr := net.JoinHostPort(host, port.Port())

	if err := applyClickHouseSchema(ctx, addr); err != nil {
		_ = container.Terminate(ctx)
		return "", nil, err
	}
	return addr, container, nil
}

// applyClickHouseSchema executes deploy/clickhouse/tier1_schema.sql statement by
// statement over the native protocol. It connects to the `default` database
// (the schema's first statement CREATEs `stellar`), retry-pings briefly to
// absorb any residual post-boot warmup, then runs each statement.
func applyClickHouseSchema(ctx context.Context, addr string) error {
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr:        []string{addr},
		Auth:        clickhouse.Auth{Database: "default"},
		DialTimeout: 10 * time.Second,
	})
	if err != nil {
		return fmt.Errorf("clickhouse: open schema conn %s: %w", addr, err)
	}
	defer func() { _ = conn.Close() }()

	if err := pingWithRetry(ctx, conn, 30*time.Second); err != nil {
		return fmt.Errorf("clickhouse: ping schema conn %s: %w", addr, err)
	}

	stmts, err := clickHouseSchemaStatements()
	if err != nil {
		return err
	}
	for i, s := range stmts {
		if err := conn.Exec(ctx, s); err != nil {
			return fmt.Errorf("clickhouse: apply schema statement %d (%.72q): %w", i, s, err)
		}
	}
	return nil
}

// clickHouseSchemaStatements reads the canonical tier-1 schema and splits it
// into individually-executable statements. ClickHouse's native protocol runs
// ONE statement per Exec, so the multi-statement file must be split — and
// robustly: several of the file's `--` comment blocks embed example SQL that
// contains `;`, which would fragment a naive split-on-semicolon. So line
// comments are stripped FIRST (the schema has no string literals containing
// `--`), then the remainder is split on `;`.
func clickHouseSchemaStatements() ([]string, error) {
	_, thisFile, _, _ := runtime.Caller(0)
	schemaPath := filepath.Join(filepath.Dir(thisFile), "..", "..", "deploy", "clickhouse", "tier1_schema.sql")
	raw, err := os.ReadFile(schemaPath)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: read schema %s: %w", schemaPath, err)
	}
	return splitSQLStatements(string(raw)), nil
}

// splitSQLStatements strips `--` line comments then splits on `;`, dropping
// empty fragments. See clickHouseSchemaStatements for why comment-stripping
// must precede the split.
func splitSQLStatements(sql string) []string {
	var noComments strings.Builder
	for _, line := range strings.Split(sql, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		noComments.WriteString(line)
		noComments.WriteByte('\n')
	}
	var out []string
	for _, stmt := range strings.Split(noComments.String(), ";") {
		if s := strings.TrimSpace(stmt); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// pingWithRetry polls conn.Ping until it succeeds or the timeout elapses. The
// container wait strategy already gates on the native port + HTTP /ping, so
// this normally returns on the first attempt; it's a cheap guard against the
// narrow window where the port is open but the server is still finishing
// startup.
func pingWithRetry(ctx context.Context, conn driver.Conn, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		if err := conn.Ping(ctx); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			return lastErr
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// dialClickHouse opens a raw native-protocol connection to the shared container
// against `database`, for the handful of test fixtures that need precise
// control the repo's write path doesn't expose (explicit ingested_at ordering,
// direct index-table seeding). Assertions still go through the repo's real
// readers — this is fixture scaffolding only.
func dialClickHouse(t *testing.T, ctx context.Context, database string) driver.Conn {
	t.Helper()
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr:        []string{clickhouseAddr(t)},
		Auth:        clickhouse.Auth{Database: database},
		DialTimeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("dial clickhouse (%s): %v", database, err)
	}
	if err := conn.Ping(ctx); err != nil {
		t.Fatalf("ping clickhouse (%s): %v", database, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}
