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

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// dialRefused is the error a hard-down ClickHouse produces on connect: a *net.OpError with Op
// "dial" and Timeout() FALSE (a refused connection answers instantly). It must not be classified
// non-retryable and rendered as an `errors/internal` 500.
func dialRefused() error {
	return &net.OpError{
		Op:     "dial",
		Net:    "tcp",
		Addr:   &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9300},
		Err:    syscall.ECONNREFUSED,
		Source: nil,
	}
}

// TestRetryableColdMiss_LakeUnreachable pins that a transport-layer failure below the query
// layer (dial refused, socket reset, broken pipe, resolution failure) is a dependency outage, not
// a bug here, and classifies retryable. Every case has Timeout()==false, so an arm matching only
// ne.Timeout() would miss them all.
func TestRetryableColdMiss_LakeUnreachable(t *testing.T) {
	ctx := context.Background()

	// Sanity: the fixture really is the non-timeout shape the finding
	// describes. If a future Go release made a refused dial report
	// Timeout()==true, a timeout-only classifier would cover it and this
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

// TestAccountState_LakeUnreachableMapsTo503 is the wire-level half of the above: with ClickHouse
// hard-down, GET /v1/accounts/{g} answers 503 with Retry-After and a detail saying the dependency
// is unreachable, not a 500 and not the read-budget "timed out" prose (a refused dial returns in
// microseconds, so blaming a slow query would misdirect an operator).
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

// TestAccountState_GateSaturationMapsTo503: a saturated detached-refresh gate returns
// clickhouse.ErrRefreshSaturated, transient backpressure that GET /v1/accounts/{g} must surface
// as a retryable 503, not a 500. A genuine internal error still maps to 500 so alerts and the
// 5xx SLA probe stay meaningful.
func TestAccountState_GateSaturationMapsTo503(t *testing.T) {
	cases := []struct {
		name       string
		readerErr  error
		wantStatus int
	}{
		{
			"gate saturation is a retryable 503",
			clickhouse.ErrRefreshSaturated,
			http.StatusServiceUnavailable,
		},
		{
			"wrapped gate saturation is still 503",
			fmt.Errorf("account %s: %w", validTestAccount, clickhouse.ErrRefreshSaturated),
			http.StatusServiceUnavailable,
		},
		{
			"genuine internal error stays 500",
			errors.New("clickhouse: code 60, table stellar.ledger_entries_current does not exist"),
			http.StatusInternalServerError,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var rec problemRecord
			reader := &saturationReader{
				capReader: &capReader{probe: &deadlineProbe{}},
				err:       tc.readerErr,
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
			if rec.status != tc.wantStatus {
				t.Fatalf("problem status = %d (%q), want %d", rec.status, rec.typeURL, tc.wantStatus)
			}
			if w.Code != tc.wantStatus {
				t.Fatalf("response code = %d, want %d", w.Code, tc.wantStatus)
			}
			switch tc.wantStatus {
			case http.StatusServiceUnavailable:
				if rec.typeURL != "https://api.stellarindex.io/errors/account-state-timeout" {
					t.Fatalf("saturation problem type = %q, want the retryable account-state-timeout 503 type", rec.typeURL)
				}
			case http.StatusInternalServerError:
				// A real bug MUST keep the generic internal type so it stays
				// distinguishable from transient backpressure in dashboards.
				if rec.typeURL != "https://api.stellarindex.io/errors/internal" {
					t.Fatalf("genuine error problem type = %q, want errors/internal", rec.typeURL)
				}
			}
		})
	}
}

// Regression: /accounts/{g}/activity served "detached refresh
// capacity saturated" as a 500 Internal error (its handler tested only
// readTimedOut), and /operations did the same with driver-level conn
// i/o timeouts. Every capacity-class error must classify retryable.
func TestRetryableColdMiss_CapacityClasses(t *testing.T) {
	ctx := context.Background()
	for name, err := range map[string]error{
		"pkg saturation sentinel":    errRefreshSaturated,
		"clickhouse saturation":      clickhouse.ErrRefreshSaturated,
		"wrapped saturation":         fmt.Errorf("explorer: %w", errRefreshSaturated),
		"deadline":                   context.DeadlineExceeded,
		"driver net timeout":         fakeNetTimeout{},
		"wrapped driver net timeout": fmt.Errorf("query processing: %w", fakeNetTimeout{}),
	} {
		if !retryableColdMiss(ctx, err) {
			t.Errorf("%s: classified as non-retryable — this renders as 500 Internal error", name)
		}
	}
	if retryableColdMiss(ctx, errors.New("column not found")) {
		t.Error("a real bug classified retryable — 503 would mask it")
	}
}
