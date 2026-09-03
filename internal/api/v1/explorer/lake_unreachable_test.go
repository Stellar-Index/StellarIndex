package explorer

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
)

// dialRefused is the exact error shape a hard-down ClickHouse produces when
// the driver tries to open a connection: a *net.OpError whose Op is "dial"
// and whose Timeout() is FALSE (a refused connection answers instantly — it
// does not time out). This is the value #371 F4 says was classified
// non-retryable and rendered as `errors/internal` 500.
func dialRefused() error {
	return &net.OpError{
		Op:     "dial",
		Net:    "tcp",
		Addr:   &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9300},
		Err:    syscall.ECONNREFUSED,
		Source: nil,
	}
}

// TestRetryableColdMiss_LakeUnreachable pins #371 F4: a TRANSPORT-layer
// failure below the query layer (dial refused, socket reset mid-query,
// broken pipe, host stopped resolving) is a dependency outage, not a bug in
// this process, and must classify retryable — the same class as the
// saturation sentinels and the driver i/o timeouts already matched.
//
// Red without the fix: pre-fix retryableColdMiss's last arm was
// `errors.As(err, &ne) && ne.Timeout()`, and every value below has
// Timeout()==false, so all seven sub-cases returned false.
func TestRetryableColdMiss_LakeUnreachable(t *testing.T) {
	ctx := context.Background()

	// Sanity: the fixture really is the non-timeout shape the finding
	// describes. If a future Go release made a refused dial report
	// Timeout()==true, the old classifier would have covered it and this
	// test would be vacuous — so assert the premise explicitly.
	var ne net.Error
	if errors.As(dialRefused(), &ne) && ne.Timeout() {
		t.Fatal("premise broken: a refused dial now reports Timeout()==true, so this test no longer exercises the F4 gap")
	}

	for name, err := range map[string]error{
		"refused dial":                dialRefused(),
		"wrapped refused dial":        fmt.Errorf("clickhouse: acquire conn: %w", dialRefused()),
		"bare ECONNREFUSED":           syscall.ECONNREFUSED,
		"connection reset mid-query":  &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET},
		"broken pipe on write":        &net.OpError{Op: "write", Net: "tcp", Err: syscall.EPIPE},
		"host no longer resolves":     &net.DNSError{Err: "no such host", Name: "clickhouse.internal", IsNotFound: true},
		"wrapped host does not exist": fmt.Errorf("dial: %w", &net.DNSError{Err: "no such host", Name: "clickhouse.internal", IsNotFound: true}),
	} {
		if !retryableColdMiss(ctx, err) {
			t.Errorf("%s: classified as non-retryable — the handler renders this as a 500 Internal error, so a ClickHouse outage is indistinguishable from a code bug", name)
		}
	}

	// The classifier must stay narrow: a real query bug is still a 500, or
	// an outage would mask it (and the 5xx alert would never fire).
	for name, err := range map[string]error{
		"missing table":  errors.New("code: 60, message: Table stellar.operations does not exist"),
		"missing column": errors.New("code: 47, message: Missing columns: 'op_index'"),
	} {
		if retryableColdMiss(ctx, err) {
			t.Errorf("%s: classified retryable — a 503 would mask a real bug", name)
		}
	}
}

// TestAccountState_LakeUnreachableMapsTo503 is the wire-level half of #371
// F4: with ClickHouse hard-down, GET /v1/accounts/{g} must answer 503 with a
// Retry-After and a detail that says the DEPENDENCY is unreachable — not the
// 500 "Internal error" it served pre-fix, and not the read-budget "timed out"
// prose either (a refused dial answered in microseconds; sending an operator
// to look for a slow query that never ran is the same class of lie the
// saturation split already exists to prevent).
//
// Red without the fix: pre-fix the handler's retryableColdMiss returned false
// for a refused dial, so it fell through to WriteProblem(…, 500) and this
// test fails on `status = 500, want 503`.
func TestAccountState_LakeUnreachableMapsTo503(t *testing.T) {
	var rec problemRecord
	reader := &saturationReader{
		capReader: &capReader{probe: &deadlineProbe{}},
		err:       fmt.Errorf("account state: %w", dialRefused()),
	}
	h := newProbeHandler(reader, nil)
	h.WriteProblem = func(w http.ResponseWriter, _ *http.Request, typeURL, title string, status int, detail string) {
		rec = problemRecord{typeURL: typeURL, title: title, status: status, detail: detail, written: true}
		w.WriteHeader(status)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/accounts/"+validTestAccount, nil)
	req.SetPathValue("g_strkey", validTestAccount)
	w := httptest.NewRecorder()
	h.AccountState(w, req)

	if !rec.written {
		t.Fatal("no problem+json written — the handler swallowed the error")
	}
	if rec.status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d: a ClickHouse outage must not be reported as an internal error", rec.status, http.StatusServiceUnavailable)
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("response code = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
	if got := w.Header().Get("Retry-After"); got != retryAfterDependencyDown {
		t.Fatalf("Retry-After = %q, want %q — a 503 that says \"retry shortly\" without a hint invites an immediate retry storm", got, retryAfterDependencyDown)
	}
	if !strings.Contains(rec.detail, "unreachable") {
		t.Fatalf("detail = %q, want it to name the dependency outage", rec.detail)
	}
	if strings.Contains(rec.detail, "didn't return within") {
		t.Fatalf("detail = %q: a refused dial did not blow the read budget — this sends operators after a slow query that never ran", rec.detail)
	}
}
