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
// statementTimeout <= 0 falls back to plain [Open] (no session timeout,
// and no plan-mode override — the two session settings ride the same
// post-connect mechanism).
//
// The serving pool ADDITIONALLY runs `SET plan_cache_mode =
// force_custom_plan` on every connection (2026-08-24, the /v1/price p95
// tail): the request path's raw-trades fallback (TradesInRange) is a
// parameterised query over the trades hypertable, and Postgres's
// prepared-statement logic flips it to a GENERIC plan after five
// executions. Building that generic plan means planning across every
// chunk of an ~870-chunk hypertable — measured 206 ms on r1 — and the
// plancache invalidates roughly once a minute in steady state
// (autovacuum/analyze on hot chunks, compression jobs, chunk DDL), so a
// steady ~5 % of serving requests paid a 250–325 ms BIND (caught
// verbatim in the slow-query log) while p50 stayed ~15 ms. Custom plans
// for the same query measure 0.2–3 ms per bind, and TimescaleDB's
// planner prunes chunks far better with known parameters. Forcing
// custom plans on the SERVING pool trades ≤3 ms of per-query planning
// for eliminating the rebuild-tail class entirely. The
// indexer/aggregator (OpenBackground) and ops pools keep the default
// plan_cache_mode: their long-lived batch statements are exactly where
// generic plans pay off.
//
// Both settings are applied via a post-connect SET run on every new
// pooled connection (a wrapping driver.Connector), rather than by
// string-munging the operator DSN — so it works identically for URL
// and keyword-form DSNs and never disturbs the configured connection
// string.
func OpenServing(ctx context.Context, dsn string, statementTimeout time.Duration) (*Store, error) {
	return openWithSessionSetup(ctx, dsn, statementTimeout, true)
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
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("timescale: pgx.ParseConfig: %w", err)
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
