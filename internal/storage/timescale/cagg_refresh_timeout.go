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

// Per-CALL bound on refresh_continuous_aggregate. The ops backfill pool has no session
// statement_timeout, so one wedged refresh would block every `-parallel` worker behind
// ingest.caggRefreshMu until SIGINT.
//
//   - refresh_continuous_aggregate refuses to run inside a transaction block, so
//     `SET LOCAL statement_timeout` is unavailable. The GUC is set at SESSION level on one pinned
//     pooled connection for exactly the CALL, then restored.
//   - statement_timeout is kept although pgx also cancels on a Go deadline: the server enforces
//     it (query_canceled, see isStatementTimeoutErr) and the connection stays healthy. The Go
//     deadline (bound + grace) is the second line for a backend that stopped answering; the
//     driver then closes that connection, never the pool.
//
// Sizing derives from the refreshed window, not the view's MinWindow, because refresh cost scales
// with the trades under it. The rate is a THROUGHPUT floor: a refresh that cannot cover its
// window at 12x real time is wedged. The floor covers sub-hour repairs where per-CALL overhead
// dominates; the ceiling stops coarse rungs padded to MinWindow (prices_1mo: 93 days) from
// computing for days.
const (
	CAGGRefreshTimeoutPerWindowHour = 5 * time.Minute
	CAGGRefreshTimeoutFloor         = 10 * time.Minute
	CAGGRefreshTimeoutCeiling       = 4 * time.Hour

	// caggRefreshDeadlineGrace is added to the SQL bound to form the Go-side deadline, which only
	// matters for a backend that stopped answering; a live one fires statement_timeout first.
	caggRefreshDeadlineGrace = time.Minute

	// caggRefreshRestoreTimeout bounds the SET that hands the connection back, on a context
	// detached from the caller's (which may already be done).
	caggRefreshRestoreTimeout = 5 * time.Second
)

// CAGGRefreshTimeout returns the statement_timeout for one refresh_continuous_aggregate CALL
// over the given window: [CAGGRefreshTimeoutPerWindowHour] per hour, clamped to
// [[CAGGRefreshTimeoutFloor], [CAGGRefreshTimeoutCeiling]]. A zero or negative window gets the
// floor; the CALL rejects that window itself, so the bound only has to exist.
func CAGGRefreshTimeout(window time.Duration) time.Duration {
	if window <= 0 {
		return CAGGRefreshTimeoutFloor
	}
	// Scale in float64 hours first so sub-hour windows keep their precision.
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

// CAGGRefreshTimeoutError is returned by [Store.RefreshContinuousAggregate] when the per-CALL
// bound fired (statement_timeout or the Go-side deadline). The trades are untouched and a re-run
// refreshes them. It unwraps to the driver error so errors.Is(context.DeadlineExceeded) and
// pgconn.PgError matching work.
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

// isStatementTimeoutErr reports whether err is the backend's statement_timeout cancellation. A
// plain statement gives SQLSTATE 57014, but refresh_continuous_aggregate re-throws it as XX000
// with the message `canceling statement due to statement timeout`, so the message is matched
// too (as internal/api/v1/envelope.go does).
func isStatementTimeoutErr(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "57014" || strings.Contains(pgErr.Message, "canceling statement due to statement timeout")
}

// refreshCAGGBounded runs one refresh_continuous_aggregate CALL on a single pinned pooled
// connection under a session statement_timeout of `timeout`, restoring the previous value before
// the connection returns to the pool (see the file comment for why SET, not SET LOCAL).
//
// The previous value is read with current_setting so a pool with its own backstop
// ([OpenBackground]) gets THAT back, not the server default a bare RESET gives. The restore runs
// on a detached context (the caller's is already done on the Go-deadline path). A failed restore
// marks the connection bad so database/sql closes it: it returns with its original timeout or
// not at all.
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
			// Discard rather than return a connection still carrying the CALL's bound.
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			if err == nil {
				err = fmt.Errorf("restore statement_timeout: %w", rerr)
			}
		}
	}()

	_, err = conn.ExecContext(ctx, q, from, to)
	return err
}
