package timescale

import (
	"context"
	"database/sql/driver"
	"errors"
	"net"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// sqlStateClass returns the 2-char class of a Postgres SQLSTATE code. The length guard keeps a
// malformed or empty code (a driver or wrapping bug) from panicking the write-fault classifier;
// an empty class matches no case and falls to each caller's safe default.
func sqlStateClass(code string) string {
	if len(code) < 2 {
		return ""
	}
	return code[:2]
}

// IsInfraError reports whether err from a write path is an INFRASTRUCTURE fault (database
// unreachable, restarting or out of connections), as opposed to a per-row DATA fault or transient
// lock contention (deadlock, serialization).
//
// This drives the sink's failure policy: an infra fault hits every row and clears only when the DB
// returns, so the sink RETRIES with backpressure rather than dropping the write. A data fault is
// permanent for its row, so the sink error-and-skips it (one bad row must not wedge the pipeline).
// Contention is left to the batch path's per-row fallback; its sorted insert order keeps it rare.
//
// Conservative by design: only clear unavailability or capacity signals return true, and anything
// unrecognised falls to error-and-skip (fail-visible, never fail-silent).
func IsInfraError(err error) bool {
	if err == nil {
		return false
	}
	// Checked FIRST: context.DeadlineExceeded also satisfies net.Error and would otherwise read
	// as a retryable dial timeout, but cancellation is shutdown and callers must stop retrying.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	// database/sql returns driver.ErrBadConn for a dead pooled connection after its own retry.
	if errors.Is(err, driver.ErrBadConn) {
		return true
	}
	// Refused / reset / i/o timeout while dialing or talking to Postgres.
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	// pgx exposes the full 5-char SQLSTATE; its first 2 chars are the class.
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
		// Any other typed pg error (constraint, numeric, check, …) is a data fault: do NOT retry.
		return false
	}
	// Some paths surface dial errors as plain fmt-wrapped strings, not a typed net.Error.
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

// IsPermanentDataError reports whether err from a write path is a DETERMINISTIC data fault that
// can NEVER succeed on retry: an integrity-constraint violation (SQLSTATE class 23) or a data
// exception (class 22). A caller gating progress on durability (the projector's cursor) may SKIP
// such a row rather than stall forever on a poison row.
//
// It is the CONSERVATIVE complement of retry, NOT the exact negation of [IsInfraError]. False (hold
// and retry) for infra faults (class 08, shutdown, capacity), transient lock contention (40P01,
// 40001, 55P03), cancellation or statement_timeout (57014), context errors, and anything
// UNRECOGNISED. True for class 22/23 and a validation reject wrapping [ErrMalformedRow].
//
// The false-default is the safe side: an unknown error must never be skipped and silently dropped;
// a stuck row then surfaces as rising projector lag and a repeated failure metric.
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
		// Classes 08, 40, 53, 55, 57 and everything else are transient: retry, don't skip.
		return false
	}
	// Non-pg errors default to transient; a data-integrity caller's safe side is retry-and-alert.
	return false
}
