package v1

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/ratelimit"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// coldFillStub answers every cold read at once. AccountStateCached charges the request's cold fill first, as
// clickhouse.ExplorerReader's cold path does (every account here is distinct, so every read is cold), and counts the reads it starts.
type coldFillStub struct {
	ExplorerReader
	stateReads atomic.Int64
}

func (*coldFillStub) ContractEventsRecent(context.Context, string, int, clickhouse.ContractEventsCursor) ([]clickhouse.ContractActivityRow, error) {
	return nil, nil
}

func (*coldFillStub) ContractActivitySummaryFor(context.Context, string, int, uint32) (clickhouse.ContractActivitySummary, bool, error) {
	return clickhouse.ContractActivitySummary{ActiveLedgersTotal: 3}, true, nil
}

func (*coldFillStub) ContractInstanceState(context.Context, string) (clickhouse.ContractInstanceState, error) {
	return clickhouse.ContractInstanceState{Known: true}, nil
}

func (*coldFillStub) RecentLedgers(context.Context, int, uint32) ([]clickhouse.LedgerHeader, error) {
	return nil, nil
}

func (s *coldFillStub) AccountStateCached(ctx context.Context, _ string) (clickhouse.AccountState, bool, error) {
	if err := ratelimit.ChargeColdFill(ctx); err != nil {
		return clickhouse.AccountState{}, false, err
	}
	s.stateReads.Add(1)
	return clickhouse.AccountState{Exists: true, HomeDomain: "issuer.example"}, false, nil
}

// TestContractDetail_RefusedPanelServedDegraded drives the real ContractDetail handler behind the real limiter with a budget that pays
// the page's events fill and one of its two parallel cold panels. Whichever panel is refused, the page is a 200 flagged degraded and
// kept out of shared caches, never a normal page that reads as "no activity".
func TestContractDetail_RefusedPanelServedDegraded(t *testing.T) {
	const cid = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
	clock := ratelimit.WithClock(func() time.Time { return time.Unix(1_700_000_000, 0) })
	for i := range 8 { // the refused panel is a race between the two; eight pages see both outcomes
		s := &Server{Options: Options{Explorer: &coldFillStub{}}, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
		h := explorerHandlerFor(s, s.Options, s.logger)
		b := ratelimit.New(nil, 1+2*ratelimit.ColdFillCost, time.Minute, clock)
		srv := middleware.RateLimitBySubject(b, nil, nil, nil)(http.HandlerFunc(h.ContractDetail))
		r := httptest.NewRequest(http.MethodGet, "/v1/contracts/"+cid, nil)
		r.SetPathValue("contract_id", cid)
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("page %d: status %d, want 200 (a refused panel degrades the page, it does not fail it)", i, w.Code)
		}
		if got := w.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("page %d: Cache-Control %q, want no-store: a refused panel was served as a normal page", i, got)
		}
	}
}

// TestBackfillHomeDomain_IssuerFillsPayAndDegrade: one subject asking for N distinct issuers' asset pages starts at most
// budget/ColdFillCost account-state fills; every other page is served degraded (folded into its stale flag) with no read started
// and no curated fallback domain, never a 429.
func TestBackfillHomeDomain_IssuerFillsPayAndDegrade(t *testing.T) {
	stub := &coldFillStub{}
	s := &Server{Options: Options{Explorer: stub}, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	const budget = 3*ratelimit.ColdFillCost + 2
	spent := 0
	ctx := ratelimit.WithColdFillCharge(context.Background(), func(n int) error {
		if spent+n > budget {
			return fmt.Errorf("budget spent: %w", ratelimit.ErrColdFillRefused)
		}
		spent += n
		return nil
	})
	const issuers = 10
	degraded := 0
	for i := range issuers {
		issuer := fmt.Sprintf("GISSUER%02d", i)
		d := AssetDetail{Issuer: &issuer}
		if s.backfillHomeDomain(ctx, &d) {
			degraded++
			if d.HomeDomain != nil {
				t.Fatalf("issuer %d: refused read served home_domain %q", i, *d.HomeDomain)
			}
			continue
		}
		if d.HomeDomain == nil || *d.HomeDomain != "issuer.example" {
			t.Fatalf("issuer %d: paid read did not fill the live home_domain", i)
		}
	}
	fills := budget / ratelimit.ColdFillCost
	if got := stub.stateReads.Load(); got != int64(fills) {
		t.Fatalf("account-state fills = %d, want %d (budget %d / %d)", got, fills, budget, ratelimit.ColdFillCost)
	}
	if degraded != issuers-fills {
		t.Fatalf("degraded pages = %d, want %d", degraded, issuers-fills)
	}
}
