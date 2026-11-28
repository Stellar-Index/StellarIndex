package v1_test

import (
	"context"
	"errors"
	"math/big"
	"net/http"
	"sync"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// yieldRatesExplorer adds the reserve rate states to ratesExplorer, recording each window it is asked for. A pool in block
// waits until release closes or the read's context ends.
type yieldRatesExplorer struct {
	*ratesExplorer
	states  map[string]map[string]clickhouse.LendingRateStates // by pool, then reserve asset
	through time.Time
	fail    string
	block   string
	release chan struct{}

	mu      sync.Mutex
	windows []time.Duration
	slacks  []time.Duration
	reads   int
}

func (e *yieldRatesExplorer) LendingHistoryCoverage(context.Context) (clickhouse.LendingHistoryCoverage, error) {
	return clickhouse.LendingHistoryCoverage{ThroughLedger: 1, ThroughTime: e.through}, nil
}

func (e *yieldRatesExplorer) LendingRateStates(ctx context.Context, pool string, _ []string, from, to time.Time, slack time.Duration, _ clickhouse.LendingHistoryCoverage) (clickhouse.LendingRateWindow, error) {
	e.mu.Lock()
	e.windows, e.slacks, e.reads = append(e.windows, to.Sub(from)), append(e.slacks, slack), e.reads+1
	e.mu.Unlock()
	switch pool {
	case e.fail:
		return clickhouse.LendingRateWindow{}, errors.New("lake down")
	case e.block:
		select {
		case <-e.release:
		case <-ctx.Done():
			return clickhouse.LendingRateWindow{}, ctx.Err()
		}
	}
	return clickhouse.LendingRateWindow{Reserves: e.states[pool]}, nil
}

var yieldDay = time.Now().UTC().Add(-6 * 24 * time.Hour).Truncate(time.Hour)

func yieldRateState(ledger uint32, bRate, dRate int64, at time.Time) *clickhouse.LendingRateState {
	return &clickhouse.LendingRateState{Ledger: ledger, CloseTime: at, BRate: big.NewInt(bRate), DRate: big.NewInt(dRate), LastTime: at}
}

// newYieldRatesFixture serves the rates fixture with the active pool's XLM reserve accrued 1.000 -> 1.001 (b_rate) and
// 1.000 -> 1.002 (d_rate) over the six days since its first state inside the window, the history derived to now.
func newYieldRatesFixture(t *testing.T) (ratesFixture, *yieldRatesExplorer) {
	t.Helper()
	f := newRatesFixture(t)
	end := yieldRateState(20, 1_001_000_000_000, 1_002_000_000_000, yieldDay.Add(6*24*time.Hour))
	e := &yieldRatesExplorer{ratesExplorer: f.explorer, through: time.Now().UTC(), release: make(chan struct{}), states: map[string]map[string]clickhouse.LendingRateStates{
		f.activePool: {f.xlmSAC: {First: yieldRateState(10, 1_000_000_000_000, 1_000_000_000_000, yieldDay), End: end}},
	}}
	f.explorer.fail = ""
	f.srv = v1.New(v1.Options{Explorer: e, Lending: f.lending, Prices: &assetKeyedPriceReader{usd: map[string]string{"native": "0.3"}}})
	return f, e
}

// TestLendingRates_YieldOverOneWindowForEveryXLMIdentity: every XLM spelling selects the same reserve, whose realised rates
// run between its two states' own last_time over the one requested window; the rest of the row is unchanged.
func TestLendingRates_YieldOverOneWindowForEveryXLMIdentity(t *testing.T) {
	f, e := newYieldRatesFixture(t)
	base := httpTestServer(t, f.srv).URL
	if _, resp := getRates(t, base, "?window=7d"); resp.Header.Get("Cache-Control") == "no-store" {
		t.Fatal("control: every read succeeded, yet the response is degraded")
	}
	for _, asset := range []string{"native", "crypto:XLM", f.xlmSAC} {
		body, resp := getRates(t, base, "?window=7d&asset="+asset)
		if resp.StatusCode != http.StatusOK || body.Data.YieldWindow != "7d" || body.Data.APYConvention != "realised" || body.Data.YieldHistoryThrough == nil {
			t.Fatalf("%s: status %d body %+v", asset, resp.StatusCode, body.Data)
		}
		row, ok := findRate(body.Data.Rates, f.activePool, f.xlmSAC)
		y := row.Yield
		if !ok || y.RealisedFrom == nil || !time.Time(*y.RealisedFrom).Equal(yieldDay) || !ptrEq(y.Supply.RealisedAPR, "0.0608") ||
			!ptrEq(y.Borrow.RealisedAPR, "0.1217") || !ptrEq(y.Supply.EmissionsAPRReason, "emission_storage_reader_unavailable") ||
			!ptrEq(y.Supply.NetYield, "0.0608") || !y.Supply.NetLowerBound {
			t.Errorf("%s: xlm yield = %+v", asset, y)
		}
		if row.SupplyAPR == nil || *row.SupplyAPR != 0.01 || !row.Obtainable {
			t.Errorf("%s: existing fields changed: %+v", asset, row)
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, w := range e.windows {
		if w != 7*24*time.Hour || e.slacks[i] != 7*time.Hour {
			t.Errorf("states read over %s with slack %s, want the one 7d window and its 7h slack", w, e.slacks[i])
		}
	}
	if e.reads > 3 { // one per pool with rows, then the cache: active, frozen and BLNT
		t.Errorf("states read %d times for four requests over three pools", e.reads)
	}
}

// TestLendingRates_YieldHistoryBehindIsStaleAndUnranked: a history derived to two days ago serves its span, marks the
// response stale and ranks nothing.
func TestLendingRates_YieldHistoryBehindIsStaleAndUnranked(t *testing.T) {
	f, e := newYieldRatesFixture(t)
	e.through = time.Now().UTC().Add(-48 * time.Hour)
	body, _ := getRates(t, httpTestServer(t, f.srv).URL, "?asset=native")
	row, _ := findRate(body.Data.Rates, f.activePool, f.xlmSAC)
	if !body.Flags.Stale {
		t.Errorf("a history two days behind must mark the response stale: %+v", body.Flags)
	}
	if !ptrEq(row.Yield.Supply.RealisedAPR, "0.0608") || row.Yield.Supply.Rankable || !containsReason(row.Yield.Supply.RankableReasons, "rate_history_behind") {
		t.Errorf("supply = %+v", row.Yield.Supply)
	}
}

// TestLendingRates_SlowLakeDegradesOnlyTheYield: a state read that never returns costs one budget, nulls that pool's yield
// with a reason and degrades the response, while every pool and existing field is still served; the timeout is then held,
// so the next request does not read the lake again.
func TestLendingRates_SlowLakeDegradesOnlyTheYield(t *testing.T) {
	const budget = 200 * time.Millisecond
	defer v1.SetLendingYieldReadBudgetForTest(budget)()
	f, e := newYieldRatesFixture(t)
	e.block = f.activePool
	defer close(e.release)
	base := httpTestServer(t, f.srv).URL
	start := time.Now()
	body, resp := getRates(t, base, "")
	time.Sleep(2 * budget) // the detached fill ends at its own budget and stores the timeout
	e.mu.Lock()
	reads := e.reads
	e.mu.Unlock()
	againAt := time.Now()
	again, _ := getRates(t, base, "")
	took := time.Since(againAt)
	e.mu.Lock()
	defer e.mu.Unlock()
	if rowAgain, _ := findRate(again.Data.Rates, f.activePool, f.xlmSAC); took > budget/2 || e.reads != reads ||
		!ptrEq(rowAgain.Yield.Supply.RealisedAPRReason, "rate_history_timed_out") {
		t.Errorf("the timeout was not held: took %s, reads %d -> %d, supply %+v", took, reads, e.reads, rowAgain.Yield.Supply)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("a blocked lake held the response %s", took)
	}
	row, ok := findRate(body.Data.Rates, f.activePool, f.xlmSAC)
	frozen, fok := findRate(body.Data.Rates, f.frozenPool, f.xlmSAC)
	if resp.StatusCode != http.StatusOK || !ok || !fok || row.SupplyAPR == nil || *row.SupplyAPR != 0.01 || len(body.Data.OmittedPools) != 0 {
		t.Fatalf("existing rows must be served: status %d rows %+v omitted %+v", resp.StatusCode, body.Data.Rates, body.Data.OmittedPools)
	}
	if !ptrEq(row.Yield.Supply.RealisedAPRReason, "rate_history_timed_out") || row.Yield.Supply.NetYield != nil || frozen.Yield.Supply.RealisedAPRReason == nil {
		t.Errorf("blocked pool yield = %+v", row.Yield.Supply)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Error("a timed-out yield read must degrade the response")
	}
}

// TestLendingRates_YieldWindowParameter defaults to 7d, refuses any other value, and without the states reader serves every
// realised rate null with its reason, undegraded.
func TestLendingRates_YieldWindowParameter(t *testing.T) {
	f := newRatesFixture(t)
	base := httpTestServer(t, f.srv).URL
	if _, resp := getRates(t, base, "?window=90d"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("window=90d: status = %d", resp.StatusCode)
	}
	body, resp := getRates(t, base, "?protocol=blend")
	row, ok := findRate(body.Data.Rates, f.activePool, f.otherToken)
	if resp.StatusCode != http.StatusOK || body.Data.YieldWindow != "7d" || !ok ||
		!ptrEq(row.Yield.Borrow.RealisedAPRReason, "rate_history_reader_unavailable") || row.Yield.Borrow.NetYield != nil ||
		row.Yield.Borrow.NetExcluded == nil || row.Yield.Borrow.Rankable || !containsReason(row.Yield.Borrow.RankableReasons, "reserve_disabled") {
		t.Errorf("default window: status %d body %+v", resp.StatusCode, body.Data)
	}
}

func ptrEq(p *string, want string) bool { return p != nil && *p == want }

func containsReason(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
