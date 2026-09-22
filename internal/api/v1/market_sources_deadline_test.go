package v1

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// deadlineMarketSourceReader fails the per-source aggregate the way the
// handler's own budget does: the driver's error WRAPPING
// context.DeadlineExceeded, arriving on a LIVE request (r.Context() is
// still alive — see handler_budget_deadline_test.go's package doc for
// why that is the state under test, not an already-expired r.Context()).
type deadlineMarketSourceReader struct{}

func (deadlineMarketSourceReader) PairSourceStats(context.Context, []string, []string) ([]timescale.SourceStats, error) {
	return nil, deadlineOnLiveRequestErr("scanSourceStats")
}

func (deadlineMarketSourceReader) AssetSourceStats(context.Context, []string) ([]timescale.SourceStats, error) {
	return nil, deadlineOnLiveRequestErr("scanSourceStats")
}

// TestHandlerOwnBudget_MarketSourcesDeadlineOnLiveRequestIs503 is the
// T049 regression guard. /v1/markets/sources had no per-handler budget
// and no handlerTimedOut branch, so a store-side deadline — however it
// fired — fell through the bare `if err != nil` as a plain 500
// market-sources-error: indistinguishable from a real bug, and booked as
// a permanent availability failure rather than the retryable capacity
// signal a deadline actually is. It must report a retryable 503 naming
// the specific endpoint that stalled — the same shape as the cold-path
// aggregates that carry their own per-call context (history.go,
// chart.go's `-timeout` types), rather than the generic
// requestTimeoutType writeProblemErr sites (twap, liquidity-pools) fall
// back to when they have no context of their own to inspect.
func TestHandlerOwnBudget_MarketSourcesDeadlineOnLiveRequestIs503(t *testing.T) {
	for _, q := range []string{
		"/v1/markets/sources?asset=native",
		"/v1/markets/sources?base=native&quote=fiat:USD",
	} {
		s := quietServer()
		s.marketSources = deadlineMarketSourceReader{}

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

// brokenMarketSourceReader fails with a plain read error carrying no
// deadline signal — the other half of the contract pinned below.
type brokenMarketSourceReader struct{}

func (brokenMarketSourceReader) PairSourceStats(context.Context, []string, []string) ([]timescale.SourceStats, error) {
	return nil, io.ErrUnexpectedEOF
}

func (brokenMarketSourceReader) AssetSourceStats(context.Context, []string) ([]timescale.SourceStats, error) {
	return nil, io.ErrUnexpectedEOF
}

// TestHandlerOwnBudget_MarketSourcesNonDeadlineFaultStays500 guards
// against satisfying the upgrade above by relabelling EVERY failure as a
// timeout, which would hide real storage faults from the 5xx dashboards
// that separate "we are broken" from "we are slow".
func TestHandlerOwnBudget_MarketSourcesNonDeadlineFaultStays500(t *testing.T) {
	s := quietServer()
	s.marketSources = brokenMarketSourceReader{}

	rec := httptest.NewRecorder()
	s.handleMarketSources(rec, httptest.NewRequest(http.MethodGet, "/v1/markets/sources?asset=native", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 — a storage fault with budget left is a real internal "+
			"error (body %s)", rec.Code, rec.Body.String())
	}
}
