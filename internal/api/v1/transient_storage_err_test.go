package v1

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// pgxDialRefused reproduces, verbatim in shape, what pgx v5 hands a handler
// when Postgres is not listening — a *pgconn.ConnectError rendered as
// "failed to connect to `…`: dial error (…)" wrapping the *net.OpError from
// the refused dial. Constructed from stdlib types so the test asserts on the
// classification rule rather than on a vendored driver's internals.
func pgxDialRefused() error {
	op := &net.OpError{
		Op:   "dial",
		Net:  "tcp",
		Addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 5432},
		Err:  syscall.ECONNREFUSED,
	}
	return fmt.Errorf("failed to connect to `host=127.0.0.1 user=si database=stellarindex`: dial error (%w)", op)
}

// TestTransientStorageErr_UnreachablePostgres pins #371 F8: every substring
// the classifier matched pre-fix ("57014", "bad connection", "broken pipe",
// "connection reset", "EOF") describes a connection that EXISTED and then
// misbehaved. None of them matches a failure to ESTABLISH one — which is
// exactly what a restarting, downed or failing-over Postgres produces, and
// is the one dependency-outage shape that hits every handler at once. It
// fell through to a 500 "Internal error": an outage that reads as a bug.
//
// Red without the fix: every "want retryable" case below returns false.
func TestTransientStorageErr_UnreachablePostgres(t *testing.T) {
	dnsErr := &net.DNSError{Err: "no such host", Name: "postgres.internal", IsNotFound: true}

	retryable := map[string]error{
		"pgx dial refused":         pgxDialRefused(),
		"bare refused dial":        &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED},
		"bare ECONNREFUSED":        syscall.ECONNREFUSED,
		"host no longer resolves":  dnsErr,
		"wrapped host unresolved":  fmt.Errorf("query issuers: %w", dnsErr),
		"string-only dial failure": errors.New("failed to connect to `host=db user=si`: dial error (dial tcp: connect: connection refused)"),
		// The pre-fix arms must all still classify — this fix ADDS a
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
) ([]timescale.SEP41TransferRow, error) {
	return nil, r.err
}

// TestSEP41Transfers_UnreachableStorageMapsTo503 is the wire-level half of
// #371 F8: with Postgres refusing connections, GET
// /v1/contracts/{id}/transfers must answer the retryable 503 its handler
// already reserves for transient storage failures — not 500.
//
// Red without the fix: transientStorageErr(pgxDialRefused()) was false, so
// the handler fell to its `sep41-transfers-error` 500 branch and this test
// fails on `status = 500, want 503`.
func TestSEP41Transfers_UnreachableStorageMapsTo503(t *testing.T) {
	srv := serverWithSEP41Reader(&unreachableTransfersReader{err: pgxDialRefused()})
	srv.logger = slog.New(slog.NewTextHandler(io.Discard, nil))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/contracts/{contract_id}/transfers", srv.handleSEP41Transfers)

	req := httptest.NewRequest(http.MethodGet, "/v1/contracts/"+validContractID+"/transfers", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusServiceUnavailable, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "sep41-transfers-transient") {
		t.Fatalf("problem type is not the transient-storage one; body: %s", rec.Body.String())
	}
}
