package timescale

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"time"

	"github.com/lib/pq"
)

// OpenServing is [Open] with a session-level `statement_timeout` applied
// to every connection in the pool. The API serving binary uses it so a
// runaway request-path query is bounded SQL-side even if Go-side context
// cancellation races (R1, audit-2026-07-16 — the systemic root behind
// the P1/C3-1/C3-2 unauth-DoS: no pool-level statement_timeout on the
// serving pool). It is the defense-in-depth backstop UNDER the app-layer
// per-request context deadline, which is the primary bound.
//
// Scope is the serving pool ONLY. The indexer/aggregator keep using
// [Open] — their heavy batch scans (per_source_gaps, source_coverage,
// row_counts, …) set their own longer `SET LOCAL statement_timeout`
// inside a transaction, which overrides this session default for exactly
// those statements. A plain request-path read (no explicit SET LOCAL)
// inherits this session default and is bounded by it.
//
// statementTimeout <= 0 falls back to plain [Open] (no session timeout).
//
// The timeout is set via a post-connect `SET statement_timeout` run on
// every new pooled connection (a wrapping driver.Connector), rather than
// by string-munging the operator DSN — so it works identically for URL
// and keyword-form DSNs and never disturbs the configured connection
// string.
func OpenServing(ctx context.Context, dsn string, statementTimeout time.Duration) (*Store, error) {
	if statementTimeout <= 0 {
		return Open(ctx, dsn)
	}

	base, err := pq.NewConnector(dsn)
	if err != nil {
		return nil, fmt.Errorf("timescale: pq.NewConnector: %w", err)
	}
	db := sql.OpenDB(&statementTimeoutConnector{
		base:      base,
		timeoutMS: statementTimeout.Milliseconds(),
	})
	configurePool(db)

	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(pctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("timescale: ping: %w", err)
	}
	return &Store{db: db}, nil
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
		// lib/pq's *conn implements ExecerContext; this guards against a
		// silent driver swap that would otherwise leave the pool
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
