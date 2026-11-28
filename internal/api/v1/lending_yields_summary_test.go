package v1_test

import (
	"math/big"
	"net/http"
	"reflect"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

type yieldsSummaryBody struct {
	Data  v1.LendingYieldsSummaryView `json:"data"`
	Flags v1.Flags                    `json:"flags"`
}

func getYieldsSummary(t *testing.T, base, query string) (yieldsSummaryBody, *http.Response) {
	t.Helper()
	resp := mustGet(t, base+"/v1/lending/yields/summary"+query)
	var body yieldsSummaryBody
	if resp.StatusCode == http.StatusOK {
		mustDecode(t, resp, &body)
	}
	return body, resp
}

// TestLendingYieldsSummary_EqualsTheRatesYieldBlock: for the same inputs every /rates row's side yield appears once in the
// summary, field for field, ranked exactly when /rates calls it rankable and otherwise listed with its reasons.
func TestLendingYieldsSummary_EqualsTheRatesYieldBlock(t *testing.T) {
	f, _ := newYieldRatesFixture(t)
	base := httpTestServer(t, f.srv).URL
	rates, _ := getRates(t, base, "?window=7d")
	sum, resp := getYieldsSummary(t, base, "?window=7d")
	if resp.StatusCode != http.StatusOK || sum.Data.YieldWindow != "7d" || sum.Data.Order != "net_yield_desc" || len(rates.Data.Rates) == 0 {
		t.Fatalf("status %d summary %+v rates %d", resp.StatusCode, sum.Data, len(rates.Data.Rates))
	}
	for name, side := range map[string]struct {
		table v1.LendingYieldsSummarySide
		pick  func(v1.LendingRate) v1.LendingSideYield
	}{
		"supply": {sum.Data.Supply, func(r v1.LendingRate) v1.LendingSideYield { return r.Yield.Supply }},
		"borrow": {sum.Data.Borrow, func(r v1.LendingRate) v1.LendingSideYield { return r.Yield.Borrow }},
	} {
		if side.table.Counts.Ranked+side.table.Counts.Unrankable != len(rates.Data.Rates) {
			t.Errorf("%s: %d ranked + %d unrankable, want every one of %d rows", name, side.table.Counts.Ranked, side.table.Counts.Unrankable, len(rates.Data.Rates))
		}
		for _, row := range rates.Data.Rates {
			want := side.pick(row)
			got, ranked, ok := findSummaryRow(side.table, row.Pool, row.Asset)
			switch {
			case !ok:
				t.Errorf("%s %s/%s: missing from the summary", name, row.Pool, row.Asset)
			case !reflect.DeepEqual(got.LendingSideYield, want):
				t.Errorf("%s %s/%s: summary %+v != rates %+v", name, row.Pool, row.Asset, got.LendingSideYield, want)
			case ranked != want.Rankable || (ranked && got.Rank == nil) || (!ranked && (got.Rank != nil || len(got.RankableReasons) == 0)):
				t.Errorf("%s %s/%s: ranked=%v rank=%v, rates rankable=%v reasons %v", name, row.Pool, row.Asset, ranked, got.Rank, want.Rankable, got.RankableReasons)
			}
		}
		assertRankedOrder(t, name, side.table)
	}
}

// TestLendingYieldsSummary_UnrankableListedWithReasons: the frozen pool's yield is known but its pool is not active, so it is
// listed unranked with that reason and counted, never ranked below the others.
func TestLendingYieldsSummary_UnrankableListedWithReasons(t *testing.T) {
	f, e := newYieldRatesFixture(t)
	st := e.states[f.activePool][f.xlmSAC]
	e.states[f.frozenPool] = map[string]clickhouse.LendingRateStates{f.xlmSAC: st}
	body, _ := getYieldsSummary(t, httpTestServer(t, f.srv).URL, "?asset=native")
	got, ranked, ok := findSummaryRow(body.Data.Supply, f.frozenPool, f.xlmSAC)
	if !ok || ranked || got.NetYield == nil || !containsReason(got.RankableReasons, "pool_not_active") {
		t.Fatalf("frozen pool supply: present=%v ranked=%v row %+v", ok, ranked, got)
	}
	if body.Data.Supply.Counts.ByReason["pool_not_active"] < 1 || body.Data.Supply.Counts.Unrankable != len(body.Data.Supply.Unrankable) {
		t.Errorf("counts %+v", body.Data.Supply.Counts)
	}
}

// TestLendingYieldsSummary_EveryXLMSpellingSelectsTheSACReserve: native, crypto:XLM and the SAC address list the same sides.
func TestLendingYieldsSummary_EveryXLMSpellingSelectsTheSACReserve(t *testing.T) {
	f, _ := newYieldRatesFixture(t)
	base := httpTestServer(t, f.srv).URL
	for _, a := range []string{"native", "crypto:XLM", f.xlmSAC} {
		body, resp := getYieldsSummary(t, base, "?asset="+a)
		got, _, ok := findSummaryRow(body.Data.Supply, f.activePool, f.xlmSAC)
		n := body.Data.Supply.Counts.Ranked + body.Data.Supply.Counts.Unrankable
		if resp.StatusCode != http.StatusOK || !ok || str(got.RealisedAPR) != "0.0608" || n != 2 {
			t.Errorf("%s: status %d sides %d active xlm %+v", a, resp.StatusCode, n, got)
		}
	}
}

func findSummaryRow(side v1.LendingYieldsSummarySide, pool, asset string) (v1.LendingYieldsSummaryRow, bool, bool) {
	for _, r := range side.Ranked {
		if r.Pool == pool && r.Asset == asset {
			return r, true, true
		}
	}
	for _, r := range side.Unrankable {
		if r.Pool == pool && r.Asset == asset {
			return r, false, true
		}
	}
	return v1.LendingYieldsSummaryRow{}, false, false
}

func assertRankedOrder(t *testing.T, name string, side v1.LendingYieldsSummarySide) {
	t.Helper()
	var prev *big.Rat
	for i, r := range side.Ranked {
		n, ok := new(big.Rat).SetString(str(r.NetYield))
		if !ok || r.Rank == nil || *r.Rank != i+1 || (prev != nil && n.Cmp(prev) > 0) {
			t.Errorf("%s ranked[%d] = %+v out of order", name, i, r)
		}
		prev = n
	}
}
