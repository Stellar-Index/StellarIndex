package timescale

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
)

// OpenServing is [Open] with a session-level `statement_timeout` on every pooled connection, the
// SQL-side backstop under the per-request deadline. Heavy scans override it with
// `SET LOCAL statement_timeout`. statementTimeout <= 0 falls back to plain [Open].
//
// The serving pool also runs `SET plan_cache_mode = force_custom_plan`: the raw-trades fallback
// (TradesInRange) flips to a GENERIC plan after five executions, and planning that across every
// chunk costs hundreds of milliseconds whenever the plan cache invalidates. Custom plans bind in
// milliseconds and prune better; batch pools keep the default, where generic plans pay off.
//
// Both settings are a post-connect SET (a wrapping driver.Connector), not a DSN edit, so URL and
// keyword-form DSNs behave alike.
func OpenServing(ctx context.Context, dsn string, statementTimeout time.Duration) (*Store, error) {
	return openWithSessionSetup(ctx, dsn, statementTimeout, true)
}

// OpenBackground is [Open] with a session-level `statement_timeout` on every pooled connection,
// for the long-running indexer and aggregator binaries, so a stuck query stops server-side even
// after its Go ctx is cancelled.
//
// The bound is deliberately GENEROUS so it only kills a true runaway; heavy batch scans set a
// longer `SET LOCAL statement_timeout` in their own transaction. One-shot ops/migrate/backfill
// paths stay on unbounded [Open]. statementTimeout <= 0 falls back to plain [Open].
func OpenBackground(ctx context.Context, dsn string, statementTimeout time.Duration) (*Store, error) {
	return openWithSessionSetup(ctx, dsn, statementTimeout, false)
}

// openWithSessionSetup is the shared implementation behind OpenServing and OpenBackground: every
// connection runs its session SETs on dial (via [statementTimeoutConnector]) and the pool is
// pinged before returning. statementTimeout <= 0 falls back to plain [Open]; the serving
// plan-mode override rides the same connector, so it also needs a positive timeout.
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

// statementTimeoutConnector wraps a driver.Connector so every freshly dialed connection runs
// `SET statement_timeout` before reaching the pool. The session GUC lasts the connection's life
// until a transaction overrides it with `SET LOCAL`.
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
