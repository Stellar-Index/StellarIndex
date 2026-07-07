package timescale

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/lib/pq"
)

func TestIsInfraError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"context canceled", context.Canceled, false},
		{"context deadline", context.DeadlineExceeded, false},
		// The 2026-07-06 incident signature.
		{"dial connection refused (string)", errors.New("dial tcp 127.0.0.1:5432: connect: connection refused"), true},
		{"wrapped connection refused", fmt.Errorf("timescale: BatchInsertTrades: %w", errors.New("connect: connection refused")), true},
		{"driver bad conn", driver.ErrBadConn, true},
		{"wrapped driver bad conn", fmt.Errorf("query: %w", driver.ErrBadConn), true},
		{"net.OpError dial", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")}, true},
		{"connection reset", errors.New("read tcp: connection reset by peer"), true},
		{"pg admin shutdown 57P01", &pq.Error{Code: "57P01", Message: "terminating connection due to administrator command"}, true},
		{"pg cannot_connect_now 57P03", &pq.Error{Code: "57P03", Message: "the database system is starting up"}, true},
		{"pg too_many_connections 53300", &pq.Error{Code: "53300"}, true},
		{"pg connection_exception class 08", &pq.Error{Code: "08006"}, true},
		{"pg starting up (string)", errors.New("pq: the database system is starting up"), true},
		// Data faults — must NOT retry.
		{"pg not-null violation 23502", &pq.Error{Code: "23502"}, false},
		{"pg check violation 23514", &pq.Error{Code: "23514"}, false},
		{"pg numeric overflow 22003", &pq.Error{Code: "22003"}, false},
		{"pg deadlock 40P01 (contention, per-row fallback)", &pq.Error{Code: "40P01"}, false},
		{"pg serialization 40001 (contention)", &pq.Error{Code: "40001"}, false},
		{"generic validation error", errors.New("timescale: InsertTrade: invalid trade: zero amount"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsInfraError(tc.err); got != tc.want {
				t.Errorf("IsInfraError(%v) = %v; want %v", tc.err, got, tc.want)
			}
		})
	}
}
