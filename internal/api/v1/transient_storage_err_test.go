package v1

import (
	"context"
	"errors"
	"fmt"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// pgxDialRefused reproduces what pgx v5 returns when Postgres isn't listening: a *pgconn.ConnectError wrapping the
// refused dial's *net.OpError. Built from stdlib types so the test asserts the classification rule, not driver internals.
func pgxDialRefused() error {
	op := &net.OpError{
		Op:   "dial",
		Net:  "tcp",
		Addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 5432},
		Err:  syscall.ECONNREFUSED,
	}
	return fmt.Errorf("failed to connect to `host=127.0.0.1 user=si database=stellarindex`: dial error (%w)", op)
}

// TestTransientStorageErr_UnreachablePostgres pins that a failure to establish a connection is retryable. The
// substrings "57014", "bad connection", "broken pipe", "connection reset", "EOF" describe a connection that existed;
// a downed or failing-over Postgres would otherwise fall through to a 500 that reads as a bug.
func TestTransientStorageErr_UnreachablePostgres(t *testing.T) {
	dnsErr := &net.DNSError{Err: "no such host", Name: "postgres.internal", IsNotFound: true}

	retryable := map[string]error{
		"pgx dial refused":         pgxDialRefused(),
		"bare refused dial":        &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED},
		"bare ECONNREFUSED":        syscall.ECONNREFUSED,
		"host no longer resolves":  dnsErr,
		"wrapped host unresolved":  fmt.Errorf("query issuers: %w", dnsErr),
		"string-only dial failure": errors.New("failed to connect to `host=db user=si`: dial error (dial tcp: connect: connection refused)"),
		// The original arms must all still classify — this fix ADDS a
		// class, it does not re-cut the existing ones.
		"pg statement cancel": errors.New("ERROR: canceling statement due to user request (SQLSTATE 57014)"),
		"driver bad conn":     errors.New("driver: bad connection"),
		"broken pipe":         errors.New("write tcp: broken pipe"),
		"unexpected EOF":      errors.New("unexpected EOF"),
	}
	for name, err := range retryable {
		if !transientStorageErr(err) {
			t.Errorf("%s: classified non-transient — the handler renders this as a 500, so a Postgres outage is indistinguishable from a code bug", name)
		}
	}

	notRetryable := map[string]error{
		"nil":              nil,
		"undefined column": errors.New(`ERROR: column "usd_volume" does not exist (SQLSTATE 42703)`),
		"scan type error":  errors.New("sql: Scan error on column index 3: converting NULL to string is unsupported"),
	}
	for name, err := range notRetryable {
		if transientStorageErr(err) {
			t.Errorf("%s: classified transient — a 503 would mask a real bug and the 5xx alert would never fire", name)
		}
	}
}

// unreachableTransfersReader fails every read with a Postgres-unreachable
// error, so the handler's status mapping can be driven without a database.
type unreachableTransfersReader struct{ err error }

func (r *unreachableTransfersReader) ListSEP41Transfers(
	_ context.Context, _, _, _ string, _ int,
) ([]timescale.SEP41TransferRow, time.Time, error) {
	return nil, time.Time{}, r.err
}
