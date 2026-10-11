package timescale

import (
	"context"
	"database/sql/driver"
	"errors"
	"net"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// sqlStateClass returns the 2-char class of a Postgres SQLSTATE code. Server
// SQLSTATEs are always 5 chars, so in practice this is just code[:2]; guarding
// the slice keeps a malformed/empty code (a driver or error-wrapping bug) from
// panicking the write-fault classifier. An empty class matches no case and
// falls through to each caller's safe default (transient / non-permanent).
func sqlStateClass(code string) string {
	if len(code) < 2 {
		return ""
	}
	return code[:2]
}

// IsInfraError reports whether err from a write path is an
// INFRASTRUCTURE fault — the database is unreachable, restarting, or
// out of connection capacity — as opposed to a per-row DATA fault
// (constraint / numeric / check violation) or transient row-lock
// contention (deadlock / serialization).
//
// The distinction drives the sink's failure policy: an infra fault
// affects every row identically and clears only when the DB comes
// back, so the sink RETRIES with backpressure rather than dropping the
// write. A data fault is permanent for the offending row, so the sink
// error-and-skips it (one bad row must not wedge the pipeline).
// Contention is left to the existing per-row fallback in the batch
// path — it is neither an unavailability signal nor a permanent row
// fault, and the batch path's sorted insert order keeps it rare.
//
// Without this predicate, a Postgres outage — `dial tcp
// 127.0.0.1:5432: connect: connection refused` — would make the trade
// sink log "insert trade failed" and drop the write while the ledger
// cursor kept advancing. This function is the gate that turns that
// drop into a blocking retry.
//
// Conservative by design: only clear unavailability/capacity signals
// return true. Anything unrecognised returns false so it falls to the
// error-and-skip path (fail-visible, never fail-silent).
func IsInfraError(err error) bool {
	if err == nil {
		return false
	}
	// Context cancellation / deadline is shutdown, not an infra fault —
	// callers stop retrying on it. Checked FIRST because
	// context.DeadlineExceeded also satisfies net.Error (Timeout()=true)
	// and would otherwise be misclassified as a retryable dial timeout.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	// database/sql surfaces a dead pooled connection as driver.ErrBadConn
	// once it has exhausted its own internal retry.
	if errors.Is(err, driver.ErrBadConn) {
		return true
	}
	// Net-level: connection refused / reset / i/o timeout while dialing
	// or talking to Postgres. *net.OpError satisfies net.Error.
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	// Postgres server-state SQLSTATEs (pgx exposes pgconn.PgError.Code as
	// the full 5-char SQLSTATE; the first 2 chars are its class).
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		if sqlStateClass(pgErr.Code) == "08" { // connection_exception family
			return true
		}
		switch pgErr.Code {
		case "57P01", // admin_shutdown       — server is shutting down
			"57P02", // crash_shutdown       — server crashed
			"57P03", // cannot_connect_now   — server still starting up
			"53300", // too_many_connections
			"53400": // configuration_limit_exceeded
			return true
		}
		// Any other typed pg error (constraint, numeric, check, …) is a
		// data fault — do NOT retry it.
		return false
	}
	// Belt-and-braces string match for driver dial errors that aren't
	// wrapped as a typed net.Error (the exact incident signature travels
	// as a plain fmt-wrapped string through database/sql in some paths).
	msg := err.Error()
	for _, s := range []string{
		"connection refused",
		"connection reset",
		"broken pipe",
		"no such host",
		"i/o timeout",
		"server closed the connection",
		"the database system is", // "… is starting up" / "… is shutting down"
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// ErrMalformedRow marks a row a store method's own pre-SQL validation
// rejected. The check is deterministic, so [IsPermanentDataError] treats
// it like a class 22/23 fault: retrying the same row can never succeed.
var ErrMalformedRow = errors.New("timescale: malformed row")

// IsPermanentDataError reports whether err from a write path is a
// DETERMINISTIC data fault that can NEVER succeed on retry: an integrity-
// constraint violation (SQLSTATE class 23, e.g. the `amount > 0` CHECK of
// migration 0096, not-null, foreign-key, unique) or a data exception (class
// 22, e.g. numeric_value_out_of_range). A caller that gates progress on
// durability (the ADR-0032 projector's cursor) may SKIP past such a row
// rather than stall forever on a poison row.
//
// It is the CONSERVATIVE complement of retry, NOT the exact negation of
// [IsInfraError]. False (hold and retry) for: infra faults (class 08,
// shutdown, capacity); transient lock contention (40P01, 40001, 55P03);
// query cancellation / statement_timeout (57014); context cancellation or
// deadline; and anything UNRECOGNISED, including non-pg errors and a
// validation error that does not wrap the sentinel. True for class 22 / 23
// and a store validation reject wrapping [ErrMalformedRow].
//
// The false-default is the safe side: an unknown error must never be
// skipped and silently dropped. A genuinely stuck row then surfaces as
// rising projector lag + a repeated failure-outcome metric, not a silent
// stall.
func IsPermanentDataError(err error) bool {
	if err == nil {
		return false
	}
	// Shutdown / cycle-deadline is transient, not a permanent data fault.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, ErrMalformedRow) {
		return true
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch sqlStateClass(pgErr.Code) { // SQLSTATE class = first 2 chars
		case "22", // data_exception (numeric out of range, invalid text rep, …)
			"23": // integrity_constraint_violation (CHECK / not-null / fk / unique)
			return true
		}
		// class 08 (connection), 40 (deadlock/serialization), 53
		// (capacity), 55 (lock_not_available), 57 (query_canceled /
		// shutdown) and everything else are transient — retry, don't skip.
		return false
	}
	// Non-pg error (net fault, driver.ErrBadConn, an unwrapped
	// validation error): default to transient. See the godoc — the
	// safe side for a data-integrity caller is retry-and-alert, not skip.
	return false
}
