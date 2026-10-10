package timescale

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
)

// OpenServing is [Open] with a session-level `statement_timeout` applied to every
// connection in the pool, so a runaway request-path query is bounded SQL-side even
// if Go-side context cancellation races. It is the backstop UNDER the app-layer
// per-request deadline, which is the primary bound.
//
// The indexer/aggregator pools get their own backstop via [OpenBackground]; the
// one-shot ops/migrate/backfill pools stay unbounded on plain [Open]. Heavy batch
// scans in a bounded pool set their own longer `SET LOCAL statement_timeout`
// inside a transaction, which overrides the session default.
//
// statementTimeout <= 0 falls back to plain [Open] (no timeout, no plan-mode
// override; both ride the same post-connect mechanism).
//
// The serving pool ADDITIONALLY runs `SET plan_cache_mode = force_custom_plan`
// for the /v1/price p95 tail: the raw-trades fallback (TradesInRange) is a
// parameterised query that Postgres flips to a GENERIC plan after five
// executions, and planning that across every hypertable chunk takes hundreds of
// milliseconds each time the plan cache invalidates (about once a minute).
// Custom plans bind in milliseconds and prune chunks better. Other pools keep the
// default: their long-lived batch statements are where generic plans pay off.
//
// Both settings are applied via a post-connect SET on every new pooled connection
// (a wrapping driver.Connector), not by editing the operator DSN, so URL and
// keyword-form DSNs behave alike.
func OpenServing(ctx context.Context, dsn string, statementTimeout time.Duration) (*Store, error) {
	return openWithSessionSetup(ctx, dsn, statementTimeout, true)
}

// OpenBackground is [Open] with a session-level `statement_timeout` applied
// to every connection in the pool, for the long-running INDEXER and
// AGGREGATOR binaries. It is the SQL-side runaway backstop: without it,
// only the serving pool self-bounds (OpenServing), so a genuinely stuck
// indexer/aggregator query would keep running server-side even after the
// Go-side ctx was cancelled.
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
// (unbounded); a global timeout there is deliberately not applied.
//
// statementTimeout <= 0 falls back to plain [Open] (no session timeout).
func OpenBackground(ctx context.Context, dsn string, statementTimeout time.Duration) (*Store, error) {
	return openWithSessionSetup(ctx, dsn, statementTimeout, false)
}

// openWithSessionSetup is the shared implementation behind OpenServing
// and OpenBackground: a pool whose every connection runs its session
// SETs on dial (via [statementTimeoutConnector]), Ping'd before
// returning. statementTimeout <= 0 falls back to plain [Open] — the
// serving plan-mode override rides the same connector, so it also
// requires a positive timeout (the serving pool always configures one;
// see StorageConfig.ServingStatementTimeout's default).
func openWithSessionSetup(ctx context.Context, dsn string, statementTimeout time.Duration, forceCustomPlans bool) (*Store, error) {
	connector, err := boundedConnector(dsn, statementTimeout, forceCustomPlans)
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
func boundedConnector(dsn string, statementTimeout time.Duration, forceCustomPlans bool) (driver.Connector, error) {
	if statementTimeout <= 0 {
		return nil, nil
	}
	cfg, err := sessionConnConfig(dsn)
	if err != nil {
		return nil, err
	}
	base := stdlib.GetConnector(*cfg)
	return &statementTimeoutConnector{
		base:             base,
		timeoutMS:        statementTimeout.Milliseconds(),
		forceCustomPlans: forceCustomPlans,
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
	// forceCustomPlans additionally sets `plan_cache_mode =
	// force_custom_plan` (serving pool only — see OpenServing).
	forceCustomPlans bool
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
	if c.forceCustomPlans {
		if _, err := execer.ExecContext(ctx, "SET plan_cache_mode = force_custom_plan", nil); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("timescale: set plan_cache_mode: %w", err)
		}
	}
	return conn, nil
}

func (c *statementTimeoutConnector) Driver() driver.Driver { return c.base.Driver() }
