package timescale

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Stellar-Index/StellarIndex/internal/domain"
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
		{"pg admin shutdown 57P01", &pgconn.PgError{Code: "57P01", Message: "terminating connection due to administrator command"}, true},
		{"pg cannot_connect_now 57P03", &pgconn.PgError{Code: "57P03", Message: "the database system is starting up"}, true},
		{"pg too_many_connections 53300", &pgconn.PgError{Code: "53300"}, true},
		{"pg connection_exception class 08", &pgconn.PgError{Code: "08006"}, true},
		// Malformed/empty SQLSTATE (a driver or error-wrapping bug): the
		// class guard must NOT panic and falls through to the safe default.
		{"pg empty code (malformed) — no panic, safe default", &pgconn.PgError{Code: ""}, false},
		{"pg one-char code (malformed) — no panic", &pgconn.PgError{Code: "0"}, false},
		{"pg starting up (string)", errors.New("failed to connect: the database system is starting up"), true},
		// Data faults — must NOT retry.
		{"pg not-null violation 23502", &pgconn.PgError{Code: "23502"}, false},
		{"pg check violation 23514", &pgconn.PgError{Code: "23514"}, false},
		{"pg numeric overflow 22003", &pgconn.PgError{Code: "22003"}, false},
		{"pg deadlock 40P01 (contention, per-row fallback)", &pgconn.PgError{Code: "40P01"}, false},
		{"pg serialization 40001 (contention)", &pgconn.PgError{Code: "40001"}, false},
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

// TestIsPermanentDataError_sorobanEventsValidation pins that a row
// InsertSorobanEventsBatch's own pre-SQL shape check rejects is
// classified PERMANENT. The check is deterministic, so an unclassified
// error there made the raw-event AsyncSink re-send the identical batch
// forever and stall the landing zone behind one malformed row, instead of
// bisecting to that row and counting it lost.
func TestIsPermanentDataError_sorobanEventsValidation(t *testing.T) {
	valid := func() domain.SorobanEventRow {
		return domain.SorobanEventRow{
			Ledger:        1,
			TxHash:        make([]byte, 32),
			ContractID:    "CBSORBANEVENTSVALIDATIONFIXTUREAAAAAAAAAAAAAAAAAAAAAAAAA",
			ContractIDHex: make([]byte, 32),
			Topic0XDR:     []byte{1},
			BodyXDR:       []byte{1},
		}
	}
	cases := []struct {
		name   string
		mutate func(r *domain.SorobanEventRow)
	}{
		{"short TxHash", func(r *domain.SorobanEventRow) { r.TxHash = r.TxHash[:5] }},
		{"empty ContractID", func(r *domain.SorobanEventRow) { r.ContractID = "" }},
		{"short ContractIDHex", func(r *domain.SorobanEventRow) { r.ContractIDHex = nil }},
		{"empty Topic0XDR", func(r *domain.SorobanEventRow) { r.Topic0XDR = nil }},
		{"empty BodyXDR", func(r *domain.SorobanEventRow) { r.BodyXDR = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bad := valid()
			tc.mutate(&bad)
			// Validation runs before any pool use, so a zero Store is
			// enough to exercise the shipped error path.
			err := (&Store{}).InsertSorobanEventsBatch(context.Background(),
				[]domain.SorobanEventRow{valid(), bad})
			if err == nil {
				t.Fatal("InsertSorobanEventsBatch accepted a malformed row")
			}
			if !IsPermanentDataError(err) {
				t.Errorf("IsPermanentDataError(%v) = false, want true: a deterministic "+
					"validation reject must be isolated, not retried forever", err)
			}
			if !IsPermanentDataError(fmt.Errorf("flush: %w", err)) {
				t.Error("the classification must survive wrapping")
			}
		})
	}

	// Only the positively-identified sentinel is permanent: an
	// unrecognised plain error keeps the retry-and-alert default.
	if IsPermanentDataError(errors.New("timescale: InsertSorobanEventsBatch: row 0 empty BodyXDR")) {
		t.Error("an unwrapped look-alike error must stay transient")
	}
}
