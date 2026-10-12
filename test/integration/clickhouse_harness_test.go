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

// Pins the ClickHouse server image for the raw-lake suite to a real tag on the line PRODUCTION runs (26.5, r1's line):
// rollup cycles carry per-statement settings (query_plan_join_swap_table) a server predating them refuses outright, as
// 24.8 did to the creators cycle.
const clickhouseImage = "clickhouse/clickhouse-server:26.5"

// The shared ClickHouse container: startup + schema apply is ~15-30s, so the whole integration binary shares ONE, started
// lazily and torn down once in TestMain (never started when only Postgres tests run). Tests stay isolated by unique keys
// (contract_id / tx_hash / ledger range) since the lake tables are ReplacingMergeTree and every read filters by them.
var (
	chOnce      sync.Once
	chContainer testcontainers.Container
	chAddr      string
	chErr       error
)

// TestMain owns the shared container's teardown and starts nothing eagerly (chOnce fires on first clickhouseAddr call), so
// Postgres-only runs never pay the ClickHouse startup cost.
func TestMain(m *testing.M) {
	code := m.Run()
	if chContainer != nil {
		_ = chContainer.Terminate(context.Background())
	}
	os.Exit(code)
}

// clickhouseAddr starts the shared container on first call (applying the Tier-1 schema) and returns its native-protocol
// host:port for the repo's clickhouse constructors.
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

// startClickHouse boots a container, waits for it to accept connections and applies deploy/clickhouse/tier1_schema.sql
// (which creates the `stellar` database and every lake table/MV). Returns the native host:port.
func startClickHouse(ctx context.Context) (string, testcontainers.Container, error) {
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image: clickhouseImage,
			// 9000 = native protocol (clickhouse-go driver); 8123 = HTTP (used
			// only for the /ping readiness probe).
			ExposedPorts: []string{"9000/tcp", "8123/tcp"},
			// With no CLICKHOUSE_USER/PASSWORD the image entrypoint restricts `default` to localhost, so a mapped-port connection
			// fails with a misleading AUTHENTICATION_FAILED. SKIP_USER_SETUP keeps the stock users.xml (default, empty password, all
			// networks) the repo's constructors expect; open access is fine for a throwaway container.
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

// Executes tier1_schema.sql statement by statement over the native protocol, connecting to `default` (the schema's first
// statement creates `stellar`) and retry-pinging first to absorb post-boot warmup.
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

// Splits the tier-1 schema into executable statements (native protocol runs ONE per Exec). Line comments are stripped
// FIRST because several `--` blocks embed example SQL containing `;` that a naive split would fragment (no string literal
// has `--`).
func clickHouseSchemaStatements() ([]string, error) {
	return clickHouseDeployStatements("tier1_schema.sql")
}

// clickHouseDeployStatements reads one deploy/clickhouse artifact and splits
// it into individually-executable statements.
func clickHouseDeployStatements(name string) ([]string, error) {
	path := filepath.Join(clickHouseDeployDir(), name)
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: read schema %s: %w", path, err)
	}
	return splitSQLStatements(string(raw)), nil
}

// clickHouseDeployDir is the repo's deploy/clickhouse directory.
func clickHouseDeployDir() string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "deploy", "clickhouse")
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

// Polls conn.Ping until success or timeout. The container wait strategy already gates on the port and HTTP /ping, so this
// is a cheap guard for the narrow window where the port is open but startup is unfinished.
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

// Raw native connection for fixtures needing control the write path lacks (explicit ingested_at ordering, direct index-
// table seeding). Assertions still go through the repo's readers.
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
