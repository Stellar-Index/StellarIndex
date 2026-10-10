package timescale

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Per-CALL bound on refresh_continuous_aggregate.
//
// The ops backfill pool is plain [Open] (no session statement_timeout), so one
// wedged refresh would block every `-parallel` worker behind
// ingest.caggRefreshMu until SIGINT instead of failing its own chunk.
//
//   - refresh_continuous_aggregate refuses to run inside a transaction block, so
//     `SET LOCAL statement_timeout` is unavailable. The GUC is set at SESSION
//     level on one pinned pooled connection for exactly the CALL, then restored to
//     its prior value ("0" on the ops pool, the connector's backstop on an
//     [OpenBackground] pool).
//   - statement_timeout is kept although pgx also cancels on a Go deadline: the
//     server enforces it (query_canceled, see isStatementTimeoutErr) and the
//     connection stays healthy. The Go deadline (bound + grace) is the second line
//     for a backend that stopped answering; the driver then closes that
//     connection, never the pool.
//
// Sizing derives from the refreshed window, not the view's MinWindow, because
// refresh cost scales with the trades under it. The rate is a THROUGHPUT floor: a
// refresh that cannot cover its window at 12x real time is wedged.
// [CAGGRefreshTimeoutPerWindowHour] is 5 min per window hour;
// [CAGGRefreshTimeoutFloor] (10 min) covers sub-hour repairs where per-CALL
// overhead dominates; [CAGGRefreshTimeoutCeiling] (4 h) stops coarse rungs padded
// to MinWindow (prices_1mo: 93 days) computing to days.
const (
	CAGGRefreshTimeoutPerWindowHour = 5 * time.Minute
	CAGGRefreshTimeoutFloor         = 10 * time.Minute
	CAGGRefreshTimeoutCeiling       = 4 * time.Hour

	// caggRefreshDeadlineGrace is added to the SQL bound to form the
	// Go-side context deadline. It exists only for a backend that has
	// stopped answering; a live backend always fires statement_timeout
	// first, so the typed error carries the SQL bound, not this.
	caggRefreshDeadlineGrace = time.Minute

	// caggRefreshRestoreTimeout bounds the SET that hands the pinned
	// connection back with its previous statement_timeout. Runs on a
	// context detached from the caller's (which may already be done).
	caggRefreshRestoreTimeout = 5 * time.Second
)

// CAGGRefreshTimeout returns the SQL statement_timeout to apply to a
// single refresh_continuous_aggregate CALL over a window of the given
// length: [CAGGRefreshTimeoutPerWindowHour] per hour of window, clamped
// to [[CAGGRefreshTimeoutFloor], [CAGGRefreshTimeoutCeiling]]. A zero or
// negative window (a caller passing to <= from) gets the floor; the
// CALL itself will reject the window, and the bound just has to exist.
func CAGGRefreshTimeout(window time.Duration) time.Duration {
	if window <= 0 {
		return CAGGRefreshTimeoutFloor
	}
	// Duration arithmetic in float64 hours: a 93-day window × 5 min
	// overflows nothing (≈ 4.6e14 ns), but the multiply-then-divide
	// order matters for sub-hour windows, so scale in hours first.
	hours := window.Hours()
	bound := time.Duration(hours * float64(CAGGRefreshTimeoutPerWindowHour))
	if bound < CAGGRefreshTimeoutFloor {
		return CAGGRefreshTimeoutFloor
	}
	if bound > CAGGRefreshTimeoutCeiling {
		return CAGGRefreshTimeoutCeiling
	}
	return bound
}

// CAGGRefreshTimeoutError is returned by [Store.RefreshContinuousAggregate]
// when the per-CALL bound fired: the backend cancelled the refresh
// (statement_timeout; see isStatementTimeoutErr) or, for a backend
// that stopped answering, the Go-side deadline did. It names the view and the window
// so the caller can log exactly which materialisation was abandoned;
// the trades under the window are untouched and a re-run refreshes
// them. Unwraps to the driver error so errors.Is(context.DeadlineExceeded)
// and pgconn.PgError matching still work on the wrapped chain.
type CAGGRefreshTimeoutError struct {
	View    string
	From    time.Time
	To      time.Time
	Timeout time.Duration
	Err     error
}

func (e *CAGGRefreshTimeoutError) Error() string {
	return fmt.Sprintf("timescale: RefreshContinuousAggregate(%s): refresh of [%s, %s] exceeded its %s bound: %v",
		e.View, e.From.UTC().Format(time.RFC3339), e.To.UTC().Format(time.RFC3339), e.Timeout, e.Err)
}

func (e *CAGGRefreshTimeoutError) Unwrap() error { return e.Err }

// isStatementTimeoutErr reports whether err is the backend's
// statement_timeout cancellation. A plain statement surfaces it as
// SQLSTATE 57014 (query_canceled); refresh_continuous_aggregate does
// NOT — the procedure runs transactions of its own and re-throws the
// cancellation from inside them as SQLSTATE XX000 (internal_error)
// with the original message, `canceling statement due to statement
// timeout`. Observed on timescale/timescaledb:2.26.4-pg15 by the
// integration test the first time it ran; a code-only match returned
// the wrapped generic error instead of the typed one. So the message
// is matched too, the way the serving envelope classifies 57014
// (internal/api/v1/envelope.go). Either way the meaning to this
// caller is the same: the refresh did not complete and was not left
// running.
func isStatementTimeoutErr(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "57014" || strings.Contains(pgErr.Message, "canceling statement due to statement timeout")
}

// refreshCAGGBounded runs one refresh_continuous_aggregate CALL on a
// single pinned pooled connection under a session statement_timeout of
// `timeout`, restoring the connection's previous value before it goes
// back to the pool. See the file comment for why the bound is a session
// SET rather than SET LOCAL, and why it is SQL-side at all.
//
// Connection hygiene, in order:
//   - the previous statement_timeout is read with current_setting so a
//     pool whose connector applied its own session backstop
//     ([OpenBackground]) gets THAT value back, not the server default
//     a bare RESET would give;
//   - the restore runs on a context detached from the caller's, because
//     on the Go-side-deadline path the caller's ctx is already done and
//     the restore would fail for that reason alone;
//   - a restore that fails for any reason marks the connection bad via
//     conn.Raw returning driver.ErrBadConn, which database/sql honours
//     by closing it rather than returning it to the pool. A connection
//     is only ever returned with its original timeout or not at all.
func (s *Store) refreshCAGGBounded(ctx context.Context, q string, from, to time.Time, timeout time.Duration) (err error) {
	ctx, cancel := context.WithTimeout(ctx, timeout+caggRefreshDeadlineGrace)
	defer cancel()

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("conn: %w", err)
	}
	defer func() { _ = conn.Close() }()

	var prev string
	if err := conn.QueryRowContext(ctx, `SELECT current_setting('statement_timeout')`).Scan(&prev); err != nil {
		return fmt.Errorf("read statement_timeout: %w", err)
	}
	// set_config takes the value as a parameter; SET does not.
	if _, err := conn.ExecContext(ctx, `SELECT set_config('statement_timeout', $1, false)`,
		fmt.Sprintf("%dms", timeout.Milliseconds())); err != nil {
		return fmt.Errorf("set statement_timeout: %w", err)
	}
	defer func() {
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), caggRefreshRestoreTimeout)
		defer rcancel()
		if _, rerr := conn.ExecContext(rctx, `SELECT set_config('statement_timeout', $1, false)`, prev); rerr != nil {
			// Never hand a connection back with the CALL's bound still
			// on it. Discard it; the pool dials a fresh one.
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			if err == nil {
				err = fmt.Errorf("restore statement_timeout: %w", rerr)
			}
		}
	}()

	_, err = conn.ExecContext(ctx, q, from, to)
	return err
}
