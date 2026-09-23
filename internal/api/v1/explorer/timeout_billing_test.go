package explorer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/usage"
)

// saturatedReader rejects every account-operations read at the detached
// refresh gate: the query never ran, so nothing was spent on the caller.
type saturatedReader struct{ *capReader }

func (r *saturatedReader) AccountOperations(context.Context, string, int, clickhouse.ExplorerCursor) ([]clickhouse.OpRow, error) {
	return nil, errRefreshSaturated
}

// billedAccountOperations serves one metered GET /v1/accounts/{g}/operations
// through the production UsageTracker and returns the status plus the
// month-to-date BILLABLE count MonthlyQuota enforces against.
func billedAccountOperations(t *testing.T, h *Handler) (int, int64) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	counter := usage.New(rdb)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/accounts/{g_strkey}/operations", h.AccountOperations)
	stamp := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			subject := auth.Subject{Tier: auth.TierAPIKey, KeyID: "kid_billing"}
			next.ServeHTTP(w, r.WithContext(auth.WithSubject(r.Context(), subject)))
		})
	}
	srv := middleware.Chain(mux, stamp, middleware.UsageTracker(counter, nil))

	w := httptest.NewRecorder()
	srv.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/accounts/"+validTestAccount+"/operations", nil))

	billable, err := counter.MonthToDate(context.Background(), "key:kid_billing")
	if err != nil {
		t.Fatal(err)
	}
	return w.Code, billable
}

// A lake read that blew its server-side budget is the most expensive shape a
// caller can request; it must debit the monthly quota like a served read, or a
// metered key that times its own reads out consumes the lake for free.
func TestExplorerReadTimeout_DebitsMonthlyQuota(t *testing.T) {
	var rec problemRecord
	code, billable := billedAccountOperations(t, newTimeoutHandler(&rec))
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 read-timeout", code)
	}
	if billable != 1 {
		t.Fatalf("month-to-date billable = %d after a timed-out read, want 1", billable)
	}
}

// A saturation 503 shares the `…-timeout` type URL but never ran the query:
// it stays unbillable (COR-05), so the billing split is by cause, not by URL.
func TestExplorerSaturation_DoesNotDebitMonthlyQuota(t *testing.T) {
	h := newProbeHandler(&saturatedReader{capReader: &capReader{probe: &deadlineProbe{}}}, nil)
	h.WriteProblem = func(w http.ResponseWriter, _ *http.Request, _, _ string, status int, _ string) {
		w.WriteHeader(status)
	}
	code, billable := billedAccountOperations(t, h)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 busy", code)
	}
	if billable != 0 {
		t.Fatalf("month-to-date billable = %d after a saturation reject, want 0", billable)
	}
}
