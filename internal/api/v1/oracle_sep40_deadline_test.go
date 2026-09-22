package v1

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// deadlinePriceReader fails every PriceReader call the way an expired
// per-call budget does: the driver's error WRAPPING
// context.DeadlineExceeded, arriving on a LIVE request (r.Context() is
// still alive — see handler_budget_deadline_test.go's package doc for why
// that is the state under test).
type deadlinePriceReader struct{}

func (deadlinePriceReader) LatestPrice(context.Context, canonical.Asset, canonical.Asset) (PriceSnapshot, []string, bool, error) {
	return PriceSnapshot{}, nil, false, deadlineOnLiveRequestErr("read price")
}

func (deadlinePriceReader) RecentClosedSnapshots(context.Context, canonical.Asset, canonical.Asset, int) ([]PriceSnapshot, error) {
	return nil, deadlineOnLiveRequestErr("read closed snapshots")
}

// TestHandlerOwnBudget_OracleSEP40DeadlineOnLiveRequestIs503 is the T015
// regression guard. /v1/oracle/lastprice, /v1/oracle/prices and
// /v1/oracle/x_last_price passed r.Context() straight to the PriceReader
// with no per-handler budget and no handlerTimedOut branch, so a
// store-side deadline fell through the bare `if err != nil` as a plain
// 500 — indistinguishable from a real bug, and booked as a permanent
// availability failure rather than the retryable capacity signal a
// deadline actually is. Each must report a retryable 503 naming the
// specific endpoint that stalled, the same contract T049 shipped for
// /v1/markets/sources.
func TestHandlerOwnBudget_OracleSEP40DeadlineOnLiveRequestIs503(t *testing.T) {
	cases := []struct {
		name       string
		query      string
		wantInType string
		handle     func(*Server, http.ResponseWriter, *http.Request)
	}{
		{
			name:       "lastprice",
			query:      "/v1/oracle/lastprice?asset=native",
			wantInType: "oracle-lastprice-timeout",
			handle:     (*Server).handleOracleLastPrice,
		},
		{
			name:       "prices",
			query:      "/v1/oracle/prices?asset=native",
			wantInType: "oracle-prices-timeout",
			handle:     (*Server).handleOraclePrices,
		},
		{
			name:       "x_last_price",
			query:      "/v1/oracle/x_last_price?base=native&quote=fiat:USD",
			wantInType: "oracle-xlastprice-timeout",
			handle:     (*Server).handleOracleXLastPrice,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := quietServer()
			s.prices = deadlinePriceReader{}

			req := httptest.NewRequest(http.MethodGet, tc.query, nil)
			if err := req.Context().Err(); err != nil {
				t.Fatalf("request context must be alive for this test to mean anything: %v", err)
			}
			rec := httptest.NewRecorder()
			tc.handle(s, rec, req)

			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503 — the handler's own budget expiring is retryable "+
					"capacity, and a 500 books it as a permanent availability failure (body %s)",
					rec.Code, rec.Body.String())
			}
			if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", cc)
			}
			var p Problem
			if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
				t.Fatalf("decode problem: %v (body %s)", err, rec.Body.String())
			}
			if !strings.Contains(p.Type, tc.wantInType) {
				t.Errorf("problem type = %q, want it to name %s", p.Type, tc.wantInType)
			}
		})
	}
}

// brokenPriceReader fails with a plain read error carrying no deadline
// signal — the other half of the contract pinned above.
type brokenPriceReader struct{}

func (brokenPriceReader) LatestPrice(context.Context, canonical.Asset, canonical.Asset) (PriceSnapshot, []string, bool, error) {
	return PriceSnapshot{}, nil, false, io.ErrUnexpectedEOF
}

func (brokenPriceReader) RecentClosedSnapshots(context.Context, canonical.Asset, canonical.Asset, int) ([]PriceSnapshot, error) {
	return nil, io.ErrUnexpectedEOF
}

// TestHandlerOwnBudget_OracleSEP40NonDeadlineFaultStays500 guards against
// satisfying the upgrade above by relabelling EVERY failure as a timeout,
// which would hide real storage faults from the 5xx dashboards that
// separate "we are broken" from "we are slow".
func TestHandlerOwnBudget_OracleSEP40NonDeadlineFaultStays500(t *testing.T) {
	s := quietServer()
	s.prices = brokenPriceReader{}

	rec := httptest.NewRecorder()
	s.handleOracleLastPrice(rec, httptest.NewRequest(http.MethodGet, "/v1/oracle/lastprice?asset=native", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 — a storage fault with budget left is a real internal "+
			"error (body %s)", rec.Code, rec.Body.String())
	}
}
