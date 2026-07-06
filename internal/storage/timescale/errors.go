package timescale

import (
	"context"
	"database/sql/driver"
	"errors"
	"net"
	"strings"

	"github.com/lib/pq"
)

// IsInfraError reports whether err from a write path is an
// INFRASTRUCTURE fault — the database is unreachable, restarting, or
// out of connection capacity — as opposed to a per-row DATA fault
// (constraint / numeric / check violation) or transient row-lock
// contention (deadlock / serialization).
//
// The distinction drives the sink's failure policy (2026-07-06
// Postgres-outage incident): an infra fault affects every row
// identically and clears only when the DB comes back, so the sink
// RETRIES with backpressure rather than dropping the write. A data
// fault is permanent for the offending row, so the sink error-and-
// skips it (one bad row must not wedge the pipeline). Contention is
// left to the existing per-row fallback in the batch path — it is
// neither an unavailability signal nor a permanent row fault, and the
// 2026-07-05 batch-sort fix already made it rare.
//
// The incident signature — `dial tcp 127.0.0.1:5432: connect:
// connection refused` — was NOT retried before this predicate existed:
// the trade sink logged "insert trade failed" and dropped the write
// while the ledger cursor kept advancing. This function is the gate
// that turns that drop into a blocking retry.
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
	// Postgres server-state SQLSTATEs (lib/pq exposes pq.Error.Code).
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		if pqErr.Code.Class() == "08" { // connection_exception family
			return true
		}
		switch pqErr.Code {
		case "57P01", // admin_shutdown       — server is shutting down
			"57P02", // crash_shutdown       — server crashed
			"57P03", // cannot_connect_now   — server still starting up
			"53300", // too_many_connections
			"53400": // configuration_limit_exceeded
			return true
		}
		// Any other typed pq error (constraint, numeric, check, …) is a
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
