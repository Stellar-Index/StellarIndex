package v1

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// The gap requestDeadlineExpired cannot close, pinned on two archetypes: a handler capping its
// OWN read with context.WithTimeout(r.Context(), 8s) (/v1/twap) and one at 12s
// (/v1/liquidity-pools). Under the shipping 15s api.request_timeout the inner budget blows
// FIRST, so r.Context() is still alive when the store returns its deadline error.
//
// The state under test is a deadline error arriving on a LIVE request, not an expired
// r.Context(), which request_deadline_problem_test.go already covers.

// deadlineOnLiveRequestErr is what a store returns when the per-call context deadlined: the
// driver's message WRAPPING context.DeadlineExceeded, as in production, so only errors.Is
// recognises it.
func deadlineOnLiveRequestErr(what string) error {
	return fmt.Errorf("%s: %w", what, context.DeadlineExceeded)
}

// deadlineHistoryReader fails TradesInRange as an expired 8s budget would, while r.Context() is still UNEXPIRED when the error
// is classified; that is the point of the test.
type deadlineHistoryReader struct{ HistoryReader }

func (deadlineHistoryReader) TradesInRange(
	context.Context, canonical.Pair, time.Time, time.Time, int,
) ([]canonical.Trade, error) {
	return nil, deadlineOnLiveRequestErr("query trades")
}

// deadlineLPReader fails the single-pool lookup the same way for the 12s
// /v1/liquidity-pools budget.
type deadlineLPReader struct{ ExplorerReader }

func (deadlineLPReader) NativeLiquidityPoolReserves(
	context.Context, []string,
) (map[string]clickhouse.NativeLiquidityPoolState, error) {
	return nil, deadlineOnLiveRequestErr("read liquidity_pool entries")
}

func quietServer() *Server {
	return &Server{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// assertRequestTimeout checks the full retryable-timeout contract: the status, the problem type
// in the BODY (a 503 saying `errors/internal` misleads the reader), and no-store (a CDN-cached
// timeout is replayed to everyone on that key for the whole TTL).
func assertRequestTimeout(t *testing.T, rec *httptest.ResponseRecorder, endpoint string) {
	t.Helper()
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("%s status = %d, want 503 — the handler's own budget expiring is retryable "+
			"capacity, and a 500 books it as a permanent availability failure (body %s)",
			endpoint, rec.Code, rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("%s Cache-Control = %q, want no-store", endpoint, cc)
	}
	// The body promises "retry shortly" and the status is retryable; without
	// a Retry-After the client picks the interval, and the usual pick is
	// "now" — a server already out of budget then gets the retry storm. The
	// explorer's retryable 503s hold this same standard.
	if ra := rec.Header().Get("Retry-After"); ra != retryAfterRequestTimeout {
		t.Errorf("%s Retry-After = %q, want %q", endpoint, ra, retryAfterRequestTimeout)
	}
	var p Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("%s decode problem: %v (body %s)", endpoint, err, rec.Body.String())
	}
	if p.Type != requestTimeoutType {
		t.Errorf("%s problem type = %q, want %q", endpoint, p.Type, requestTimeoutType)
	}
	if p.Status != http.StatusServiceUnavailable {
		t.Errorf("%s problem.status = %d, want 503", endpoint, p.Status)
	}
}

func TestHandlerOwnBudget_TWAPDeadlineOnLiveRequestIs503(t *testing.T) {
	s := quietServer()
	s.History = deadlineHistoryReader{}

	req := httptest.NewRequest(http.MethodGet, "/v1/twap?base=native&quote=fiat:USD", nil)
	if err := req.Context().Err(); err != nil {
		t.Fatalf("request context must be alive for this test to mean anything: %v", err)
	}
	rec := httptest.NewRecorder()
	s.handleTWAP(rec, req)

	assertRequestTimeout(t, rec, "/v1/twap")
}

func TestHandlerOwnBudget_LiquidityPoolDeadlineOnLiveRequestIs503(t *testing.T) {
	s := quietServer()
	s.Explorer = deadlineLPReader{}

	const poolHex = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	req := httptest.NewRequest(http.MethodGet, "/v1/liquidity-pools?pool="+poolHex, nil)
	if err := req.Context().Err(); err != nil {
		t.Fatalf("request context must be alive for this test to mean anything: %v", err)
	}
	rec := httptest.NewRecorder()
	s.handleLiquidityPools(rec, req)

	assertRequestTimeout(t, rec, "/v1/liquidity-pools")
}

// deadlineTokenSupplyReader fails the supply_flows sum the way the
// handler's own 8s ceiling does.
type deadlineTokenSupplyReader struct{ TokenSupplyReader }

func (deadlineTokenSupplyReader) TokenSupply(
	context.Context, string,
) (clickhouse.TokenSupply, error) {
	return clickhouse.TokenSupply{}, deadlineOnLiveRequestErr("sum supply_flows")
}

// The 502 leg of the upgrade: /v1/assets/{asset_id}/supply reports a failed lake read as 502, so our own 8s deadline must become
// a retryable 503, not read as an upstream fault.
func TestHandlerOwnBudget_AssetSupplyDeadlineOnLiveRequestIs503(t *testing.T) {
	s := quietServer()
	s.TokenSupply = deadlineTokenSupplyReader{}

	req := httptest.NewRequest(http.MethodGet, "/v1/assets/"+supplyContractID+"/supply", nil)
	req.SetPathValue("asset_id", supplyContractID)
	if err := req.Context().Err(); err != nil {
		t.Fatalf("request context must be alive for this test to mean anything: %v", err)
	}
	rec := httptest.NewRecorder()
	s.handleAssetSupply(rec, req)

	assertRequestTimeout(t, rec, "/v1/assets/{asset_id}/supply")
}

// The other half of the contract, on the same two handlers: a plain storage fault carrying NO
// deadline is still a 500. Otherwise relabelling every internal error would satisfy the upgrade
// above and hide real faults from the 5xx dashboards.
type brokenHistoryReader struct{ HistoryReader }

func (brokenHistoryReader) TradesInRange(
	context.Context, canonical.Pair, time.Time, time.Time, int,
) ([]canonical.Trade, error) {
	return nil, io.ErrUnexpectedEOF
}

func TestHandlerOwnBudget_NonDeadlineFaultStays500(t *testing.T) {
	s := quietServer()
	s.History = brokenHistoryReader{}

	rec := httptest.NewRecorder()
	s.handleTWAP(rec, httptest.NewRequest(http.MethodGet, "/v1/twap?base=native&quote=fiat:USD", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 — a storage fault with budget left is a real internal "+
			"error (body %s)", rec.Code, rec.Body.String())
	}
}

// A genuinely failed lake read on the supply endpoint still returns 502. Pins the upgrade to the deadline, not to any read error,
// so operators still see the status that points at ClickHouse.
type brokenTokenSupplyReader struct{ TokenSupplyReader }

func (brokenTokenSupplyReader) TokenSupply(
	context.Context, string,
) (clickhouse.TokenSupply, error) {
	return clickhouse.TokenSupply{}, io.ErrUnexpectedEOF
}

func TestHandlerOwnBudget_AssetSupplyNonDeadlineFaultStays502(t *testing.T) {
	s := quietServer()
	s.TokenSupply = brokenTokenSupplyReader{}

	req := httptest.NewRequest(http.MethodGet, "/v1/assets/"+supplyContractID+"/supply", nil)
	req.SetPathValue("asset_id", supplyContractID)
	rec := httptest.NewRecorder()
	s.handleAssetSupply(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 — a lake read that actually failed is an upstream fault "+
			"(body %s)", rec.Code, rec.Body.String())
	}
}

// TestHandlerOwnBudget_MarketSourcesDeadlineOnLiveRequestIs503: a store-side deadline in /v1/markets/sources must be a retryable
// 503 naming the endpoint (like the other cold-path aggregates), not a permanent-looking 500.
func TestHandlerOwnBudget_MarketSourcesDeadlineOnLiveRequestIs503(t *testing.T) {
	for _, q := range []string{
		"/v1/markets/sources?asset=native",
		"/v1/markets/sources?base=native&quote=fiat:USD",
	} {
		s := quietServer()
		s.MarketSources = deadlineMarketSourceReader{}

		req := httptest.NewRequest(http.MethodGet, q, nil)
		if err := req.Context().Err(); err != nil {
			t.Fatalf("%s: request context must be alive for this test to mean anything: %v", q, err)
		}
		rec := httptest.NewRecorder()
		s.handleMarketSources(rec, req)

		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: status = %d, want 503 — the handler's own budget expiring is retryable "+
				"capacity, and a 500 books it as a permanent availability failure (body %s)",
				q, rec.Code, rec.Body.String())
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("%s: Cache-Control = %q, want no-store", q, cc)
		}
		var p Problem
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
			t.Fatalf("%s: decode problem: %v (body %s)", q, err, rec.Body.String())
		}
		if !strings.Contains(p.Type, "market-sources-timeout") {
			t.Errorf("%s: problem type = %q, want it to name market-sources-timeout", q, p.Type)
		}
	}
}

// TestHandlerOwnBudget_MarketSourcesNonDeadlineFaultStays500 guards against relabelling every failure as a timeout, which would
// hide real storage faults from the 5xx dashboards.
func TestHandlerOwnBudget_MarketSourcesNonDeadlineFaultStays500(t *testing.T) {
	s := quietServer()
	s.MarketSources = brokenMarketSourceReader{}

	rec := httptest.NewRecorder()
	s.handleMarketSources(rec, httptest.NewRequest(http.MethodGet, "/v1/markets/sources?asset=native", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 — a storage fault with budget left is a real internal "+
			"error (body %s)", rec.Code, rec.Body.String())
	}
}

// TestHandlerOwnBudget_OracleSEP40DeadlineOnLiveRequestIs503: a store-side deadline in /v1/oracle/lastprice, /prices and
// /x_last_price must be a retryable 503 naming the endpoint, not a plain 500.
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
			s.Prices = deadlinePriceReader{}

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

// TestHandlerOwnBudget_OracleSEP40NonDeadlineFaultStays500 guards against relabelling every failure as a timeout, which would
// hide real storage faults from the 5xx dashboards.
func TestHandlerOwnBudget_OracleSEP40NonDeadlineFaultStays500(t *testing.T) {
	s := quietServer()
	s.Prices = brokenPriceReader{}

	rec := httptest.NewRecorder()
	s.handleOracleLastPrice(rec, httptest.NewRequest(http.MethodGet, "/v1/oracle/lastprice?asset=native", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 — a storage fault with budget left is a real internal "+
			"error (body %s)", rec.Code, rec.Body.String())
	}
}
