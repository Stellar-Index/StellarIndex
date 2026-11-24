package v1

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
)

func f64p(f float64) *float64 { return &f }

// An unpriced reserve leaves every figure, and the borrowed total says it is a lower bound.
func TestSummarizeReserves_UnpricedReserveIsExcluded(t *testing.T) {
	got := summarizeReserves(LendingPoolReservesView{
		TVLUSD: strp("300.00"), LowerBound: true,
		Reserves: []ReserveView{
			{SuppliedUSD: strp("100.00"), BorrowedUSD: strp("50.00"), SupplyAPR: f64p(0.02), BorrowAPR: f64p(0.10)},
			{SuppliedUSD: strp("200.00"), BorrowedUSD: strp("50.00"), SupplyAPR: f64p(0.05), BorrowAPR: f64p(0.20)},
			{SupplyAPR: f64p(9), BorrowAPR: f64p(9)},
		},
	})
	if got.TVLUSD == nil || *got.TVLUSD != "300.00" || !got.LowerBound {
		t.Errorf("tvl = %v lower=%v, want the view's 300.00 lower bound", got.TVLUSD, got.LowerBound)
	}
	if got.BorrowedUSD == nil || *got.BorrowedUSD != "100.00" || !got.BorrowedLowerBound {
		t.Errorf("borrowed = %v lower=%v, want 100.00 lower bound", got.BorrowedUSD, got.BorrowedLowerBound)
	}
	if got.PricedReserveCount != 2 || got.ReserveCount != 3 {
		t.Errorf("priced/total = %d/%d, want 2/3", got.PricedReserveCount, got.ReserveCount)
	}
	if got.UtilizationPct == nil || *got.UtilizationPct < 33.33 || *got.UtilizationPct > 33.34 {
		t.Errorf("utilization = %v, want 100/300 = 33.33%%", got.UtilizationPct)
	}
	if got.SupplyAPR == nil || *got.SupplyAPR < 0.03999 || *got.SupplyAPR > 0.04001 {
		t.Errorf("supply apr = %v, want (100*0.02+200*0.05)/300 = 0.04", got.SupplyAPR)
	}
	if got.BorrowAPR == nil || *got.BorrowAPR < 0.14999 || *got.BorrowAPR > 0.15001 {
		t.Errorf("borrow apr = %v, want 0.15", got.BorrowAPR)
	}
}

// No priced reserve: nulls, never zeros.
func TestSummarizeReserves_NothingPriced(t *testing.T) {
	got := summarizeReserves(LendingPoolReservesView{Reserves: []ReserveView{{SupplyAPR: f64p(0.1)}}})
	if got.TVLUSD != nil || got.BorrowedUSD != nil || got.UtilizationPct != nil || got.SupplyAPR != nil || got.BorrowAPR != nil {
		t.Errorf("summary = %+v, want every figure null", got)
	}
}

// Borrowed sums exactly past float precision.
func TestSummarizeReserves_BorrowedIsExact(t *testing.T) {
	got := summarizeReserves(LendingPoolReservesView{Reserves: []ReserveView{
		{SuppliedUSD: strp("90071992547409930.01"), BorrowedUSD: strp("90071992547409930.01")},
		{SuppliedUSD: strp("0.01"), BorrowedUSD: strp("0.01")},
	}})
	if got.BorrowedUSD == nil || *got.BorrowedUSD != "90071992547409930.02" || got.BorrowedLowerBound {
		t.Errorf("borrowed = %v lower=%v, want 90071992547409930.02", got.BorrowedUSD, got.BorrowedLowerBound)
	}
}

// peek on an absent key starts one fill and misses; a second peek and a blocked get join it.
func TestSWRCache_PeekMissStartsOneFill(t *testing.T) {
	c := newSWRCache[int](nil, "test", "test-fill", time.Minute, 0)
	release := make(chan struct{})
	var fills atomic.Int64
	up := func(context.Context) (int, error) { fills.Add(1); <-release; return 7, nil }

	if _, _, _, ok := c.peek("op", "k", up); ok {
		t.Fatal("first peek hit, want a miss")
	}
	done := make(chan int)
	go func() { v, _, _, _ := c.get(context.Background(), "op", "k", up); done <- v }()
	if _, _, _, ok := c.peek("op", "k", up); ok {
		t.Fatal("peek during the first fill hit, want a miss")
	}
	close(release)
	if v := <-done; v != 7 {
		t.Fatalf("get = %d, want 7", v)
	}
	if v, _, stale, ok := c.peek("op", "k", up); !ok || v != 7 || stale {
		t.Fatalf("peek after fill = %d ok=%v stale=%v, want fresh 7", v, ok, stale)
	}
	if n := fills.Load(); n != 1 {
		t.Errorf("fills = %d, want 1", n)
	}
}

// An expired entry: two peeks serve it, unflagged while its one healthy refresh runs inside the bound; the next peek is fresh.
func TestSWRCache_PeekStaleRefreshesOnce(t *testing.T) {
	const ttl = 20 * time.Millisecond
	clk := &swrTestClock{t: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
	c := newSWRCache[int](nil, "test", "test-fill", ttl, 0)
	c.now = clk.now
	if _, _, _, err := c.get(context.Background(), "op", "k", func(context.Context) (int, error) { return 1, nil }); err != nil {
		t.Fatal(err)
	}
	clk.add(ttl + ttl/2)
	release := make(chan struct{})
	var fills atomic.Int64
	up := func(context.Context) (int, error) { fills.Add(1); <-release; return 2, nil }
	for range 2 {
		if v, _, stale, ok := c.peek("op", "k", up); !ok || v != 1 || stale {
			t.Fatalf("peek = %d ok=%v stale=%v, want the expired 1 unflagged", v, ok, stale)
		}
	}
	close(release)
	fresh := func() bool { v, _, stale, ok := c.peek("op", "k", up); return ok && v == 2 && !stale }
	if !waitFor(time.Second, fresh) {
		t.Fatal("peek never served the refreshed value")
	}
	if n := fills.Load(); n != 1 {
		t.Errorf("refreshes = %d, want 1", n)
	}
}

type failingPoolVersion struct{ LendingReader }

func (failingPoolVersion) BlendPoolVersion(context.Context, string) (blend.PoolVersion, error) {
	return 0, errors.New("lake down")
}

// Every failed reserves build is memoised under a client-supplied pool id, so expired entries must not accumulate.
func TestLendingSummaryFailure_ExpiredEntriesArePruned(t *testing.T) {
	s := New(Options{Explorer: struct{ ExplorerReader }{}, Lending: failingPoolVersion{}})
	s.lendingSummaryFailed = map[string]time.Time{}
	for i := range 2 * lendingReservesMaxEntries {
		s.lendingSummaryFailed["junk"+strconv.Itoa(i)] = time.Now().Add(-lendingSummaryRetry - time.Second)
	}
	pool := "CAAQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABDQF"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/lending/pools/"+pool+"/reserves", nil))
	if rec.Code < 500 {
		t.Fatalf("status = %d, want a failed build: %s", rec.Code, rec.Body)
	}
	s.lendingSummaryMu.Lock()
	n := len(s.lendingSummaryFailed)
	_, held := s.lendingSummaryFailed[pool]
	s.lendingSummaryMu.Unlock()
	if n != 1 || !held {
		t.Fatalf("failure memo holds %d entries (pool held %v), want only the fresh failure", n, held)
	}
}

// The failure memo holds off a rebuild only inside the retry window.
func TestLendingSummaryFailure_ExpiresAfterRetry(t *testing.T) {
	s := &Server{}
	s.noteLendingSummaryFailure("p")
	if !s.lendingSummaryFailedRecently("p") {
		t.Fatal("fresh failure not held")
	}
	s.lendingSummaryFailed["p"] = time.Now().Add(-lendingSummaryRetry - time.Second)
	if s.lendingSummaryFailedRecently("p") {
		t.Error("failure older than the retry window still held")
	}
}
