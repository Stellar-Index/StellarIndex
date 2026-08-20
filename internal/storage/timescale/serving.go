package timescale

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// OpenServing is [Open] with a session-level `statement_timeout` applied
// to every connection in the pool. The API serving binary uses it so a
// runaway request-path query is bounded SQL-side even if Go-side context
// cancellation races (R1, audit-2026-07-16 — the systemic root behind
// the P1/C3-1/C3-2 unauth-DoS: no pool-level statement_timeout on the
// serving pool). It is the defense-in-depth backstop UNDER the app-layer
// per-request context deadline, which is the primary bound.
//
// The indexer/aggregator pools get their own generous session backstop
// via [OpenBackground] (REC-08); the one-shot ops/migrate/heavy-backfill
// pools stay unbounded on plain [Open]. In every bounded pool the heavy
// batch scans (per_source_gaps, source_coverage, row_counts, …) set their
// own longer `SET LOCAL statement_timeout` inside a transaction, which
// overrides the session default for exactly those statements. A plain
// request-path read (no explicit SET LOCAL) inherits this session default
// and is bounded by it.
//
// statementTimeout <= 0 falls back to plain [Open] (no session timeout).
//
// The timeout is set via a post-connect `SET statement_timeout` run on
// every new pooled connection (a wrapping driver.Connector), rather than
// by string-munging the operator DSN — so it works identically for URL
// and keyword-form DSNs and never disturbs the configured connection
// string.
func OpenServing(ctx context.Context, dsn string, statementTimeout time.Duration) (*Store, error) {
	return openWithStatementTimeout(ctx, dsn, statementTimeout)
}

// OpenBackground is [Open] with a session-level `statement_timeout` applied
// to every connection in the pool, for the long-running INDEXER and
// AGGREGATOR binaries. It is the SQL-side runaway backstop for REC-08
// (audit-2026-08-14): before it, only the serving pool self-bounded
// (OpenServing), so a genuinely stuck indexer/aggregator query kept running
// server-side even after the Go-side ctx was cancelled.
//
// The bound is deliberately GENEROUS (see StorageConfig.BackgroundStatementTimeout)
// so it only ever kills a true runaway. The heavy batch scans
// (per_source_gaps, source_coverage, row_counts, sep41_supply_events, …)
// open a transaction and `SET LOCAL statement_timeout` to their own longer
// value, which OVERRIDES this session default for exactly those statements —
// so this backstop never clips legitimate heavy work.
//
// It shares OpenServing's post-connect SET mechanism but is a distinct
// constructor so the two call-sites read their own intent (DoS backstop vs
// runaway backstop) and draw their timeout from their own config field. The
// one-shot ops/migrate/heavy-backfill paths keep using plain [Open]
// (unbounded) — a global timeout there was the rejected prior fix.
//
// statementTimeout <= 0 falls back to plain [Open] (no session timeout).
func OpenBackground(ctx context.Context, dsn string, statementTimeout time.Duration) (*Store, error) {
	return openWithStatementTimeout(ctx, dsn, statementTimeout)
}

// openWithStatementTimeout is the shared implementation behind OpenServing
// and OpenBackground: a pool whose every connection runs `SET
// statement_timeout` on dial (via [statementTimeoutConnector]), Ping'd
// before returning. statementTimeout <= 0 falls back to plain [Open].
func openWithStatementTimeout(ctx context.Context, dsn string, statementTimeout time.Duration) (*Store, error) {
	connector, err := boundedConnector(dsn, statementTimeout)
	if err != nil {
		return nil, err
	}
	if connector == nil {
		return Open(ctx, dsn)
	}
	db := sql.OpenDB(connector)
	configurePool(db)

	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(pctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("timescale: ping: %w", err)
	}
	return &Store{db: db}, nil
}

// boundedConnector builds the SET-statement_timeout-on-connect driver
// connector for a positive timeout, or returns (nil, nil) to signal "no
// bound — use plain [Open]". Split out so the timeout-arithmetic and the
// bounded/unbounded decision are unit-testable without a live Postgres.
func boundedConnector(dsn string, statementTimeout time.Duration) (driver.Connector, error) {
	if statementTimeout <= 0 {
		return nil, nil
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("timescale: pgx.ParseConfig: %w", err)
	}
	base := stdlib.GetConnector(*cfg)
	return &statementTimeoutConnector{
		base:      base,
		timeoutMS: statementTimeout.Milliseconds(),
	}, nil
}

// statementTimeoutConnector wraps a driver.Connector so every freshly
// dialed connection runs `SET statement_timeout` before it is handed to
// the pool. The GUC is a session parameter — it persists for the life of
// the connection and applies to every subsequent statement until a
// transaction overrides it with `SET LOCAL`.
type statementTimeoutConnector struct {
	base      driver.Connector
	timeoutMS int64
}

func (c *statementTimeoutConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	execer, ok := conn.(driver.ExecerContext)
	if !ok {
		// pgx stdlib's *Conn implements ExecerContext; this guards against
		// a silent driver swap that would otherwise leave the pool
		// unbounded. Fail the connection rather than pretend the timeout
		// is in force.
		_ = conn.Close()
		return nil, fmt.Errorf("timescale: driver conn %T lacks ExecerContext; cannot set statement_timeout", conn)
	}
	// SET does not accept bind parameters, so the value is rendered into
	// the statement directly. It is an int64 (milliseconds) derived from
	// a config Duration — never request/user input — so there is no
	// injection surface.
	stmt := fmt.Sprintf("SET statement_timeout = %d", c.timeoutMS)
	if _, err := execer.ExecContext(ctx, stmt, nil); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("timescale: set statement_timeout: %w", err)
	}
	return conn, nil
}

func (c *statementTimeoutConnector) Driver() driver.Driver { return c.base.Driver() }
