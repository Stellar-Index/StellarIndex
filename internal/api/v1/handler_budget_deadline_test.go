package v1

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// The gap requestDeadlineExpired cannot close, pinned on the two
// archetypes: a handler that caps its OWN read with
// context.WithTimeout(r.Context(), 8s) — /v1/twap — and one at 12s —
// /v1/liquidity-pools. Under the shipping 15s api.request_timeout the
// inner budget always blows FIRST, so r.Context() is still alive when the
// store returns its deadline error. That is the dominant shape: every
// request-derived budget in this package is 3-12s.
//
// The state under test is therefore a deadline error arriving on a LIVE
// request — not an expired r.Context(), which request_deadline_problem_
// test.go already covers, and which these two handlers never reach first.

// deadlineOnLiveRequestErr is what a store hands back when the per-call
// context deadlined: the driver's own message WRAPPING
// context.DeadlineExceeded. Wrapped rather than bare, because that is how
// it arrives in production and because errors.Is is the only thing that
// makes the wrapped form legible.
func deadlineOnLiveRequestErr(what string) error {
	return fmt.Errorf("%s: %w", what, context.DeadlineExceeded)
}

// deadlineHistoryReader fails TradesInRange the way an expired 8s budget
// does. It asserts nothing about the context it is passed — the test's
// whole point is that r.Context() is UNEXPIRED at the moment the error is
// classified.
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

// assertRequestTimeout checks the full retryable-timeout contract: the
// status, the problem type in the BODY (a 503 whose body still says
// `errors/internal` sends the reader to the opposite conclusion), and
// no-store (a timeout cached by a CDN is replayed to everyone on that
// key for the whole TTL).
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
	s.history = deadlineHistoryReader{}

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
	s.explorer = deadlineLPReader{}

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

// The 502 leg of the same upgrade, and the only handler family that
// raises one. /v1/assets/{asset_id}/supply reports a failed lake read as
// StatusBadGateway — "our upstream broke" — so an 8s ceiling this process
// imposed rendered as "Supply read failed" and sent the reader looking at
// ClickHouse for a bound ClickHouse never set. Same reasoning as the 500
// legs above: the deadline is neither this server's fault nor its
// upstream's, and it clears on retry.
func TestHandlerOwnBudget_AssetSupplyDeadlineOnLiveRequestIs503(t *testing.T) {
	s := quietServer()
	s.tokenSupply = deadlineTokenSupplyReader{}

	req := httptest.NewRequest(http.MethodGet, "/v1/assets/"+supplyContractID+"/supply", nil)
	req.SetPathValue("asset_id", supplyContractID)
	if err := req.Context().Err(); err != nil {
		t.Fatalf("request context must be alive for this test to mean anything: %v", err)
	}
	rec := httptest.NewRecorder()
	s.handleAssetSupply(rec, req)

	assertRequestTimeout(t, rec, "/v1/assets/{asset_id}/supply")
}

// The other half of the contract, on the same two handlers: a plain
// storage fault carrying NO deadline is still a 500. Without this the
// upgrade above could be satisfied by relabelling every internal error,
// which would hide real faults from the 5xx dashboards that separate "we
// are broken" from "we are slow".
type brokenHistoryReader struct{ HistoryReader }

func (brokenHistoryReader) TradesInRange(
	context.Context, canonical.Pair, time.Time, time.Time, int,
) ([]canonical.Trade, error) {
	return nil, io.ErrUnexpectedEOF
}

func TestHandlerOwnBudget_NonDeadlineFaultStays500(t *testing.T) {
	s := quietServer()
	s.history = brokenHistoryReader{}

	rec := httptest.NewRecorder()
	s.handleTWAP(rec, httptest.NewRequest(http.MethodGet, "/v1/twap?base=native&quote=fiat:USD", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 — a storage fault with budget left is a real internal "+
			"error (body %s)", rec.Code, rec.Body.String())
	}
}

// And on the 502 leg: a lake read that genuinely failed still attributes
// the failure upstream. Pins the supply upgrade to the DEADLINE rather
// than to "any supply read error", which would erase the one status that
// tells an operator to go look at ClickHouse.
type brokenTokenSupplyReader struct{ TokenSupplyReader }

func (brokenTokenSupplyReader) TokenSupply(
	context.Context, string,
) (clickhouse.TokenSupply, error) {
	return clickhouse.TokenSupply{}, io.ErrUnexpectedEOF
}

func TestHandlerOwnBudget_AssetSupplyNonDeadlineFaultStays502(t *testing.T) {
	s := quietServer()
	s.tokenSupply = brokenTokenSupplyReader{}

	req := httptest.NewRequest(http.MethodGet, "/v1/assets/"+supplyContractID+"/supply", nil)
	req.SetPathValue("asset_id", supplyContractID)
	rec := httptest.NewRecorder()
	s.handleAssetSupply(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 — a lake read that actually failed is an upstream fault "+
			"(body %s)", rec.Code, rec.Body.String())
	}
}
