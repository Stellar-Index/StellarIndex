package timescale

import (
	"context"
	"database/sql/driver"
	"errors"
	"net"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// sqlStateClass returns a SQLSTATE's 2-char class; the length guard keeps a malformed code from panicking the write-fault classifier.
func sqlStateClass(code string) string {
	if len(code) < 2 {
		return ""
	}
	return code[:2]
}

// IsInfraError reports whether a write-path err is an INFRASTRUCTURE fault (database unreachable, restarting or out of connections) rather than a per-row data fault or lock contention, which drives the sink's failure policy.
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

// ErrMalformedRow marks a row a store method's pre-SQL validation rejected; deterministic, so [IsPermanentDataError] treats it as permanent.
var ErrMalformedRow = errors.New("timescale: malformed row")

// IsPermanentDataError reports whether a write-path err is a DETERMINISTIC data fault (SQLSTATE class 22 or 23) that can never succeed on retry, so a durability-gated caller such as the projector cursor may skip the row.
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
