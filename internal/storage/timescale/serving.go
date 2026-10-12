package timescale

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
)

// OpenServing is Open with a session-level statement_timeout on every pooled connection as the SQL-side backstop under the request deadline; statementTimeout <= 0 falls back to plain Open.
func OpenServing(ctx context.Context, dsn string, statementTimeout time.Duration) (*Store, error) {
	return openWithSessionSetup(ctx, dsn, statementTimeout, true)
}

// OpenBackground is Open with a deliberately generous session statement_timeout for long-running indexer and aggregator binaries, so a stuck query stops server-side even after its ctx is cancelled.
func OpenBackground(ctx context.Context, dsn string, statementTimeout time.Duration) (*Store, error) {
	return openWithSessionSetup(ctx, dsn, statementTimeout, false)
}

// openWithSessionSetup backs OpenServing and OpenBackground: session SETs run on dial via [statementTimeoutConnector]. statementTimeout <= 0
// falls back to plain [Open], and the plan-mode override rides the same connector so it needs a positive timeout too.
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

// boundedConnector builds the SET-statement_timeout-on-connect connector, or (nil, nil) for "no bound, use [Open]"; split out to unit-test
// without Postgres.
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

// statementTimeoutConnector runs `SET statement_timeout` on every new connection; a transaction can override it with `SET LOCAL`.
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
