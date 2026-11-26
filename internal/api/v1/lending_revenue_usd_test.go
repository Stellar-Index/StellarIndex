package v1_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

func rawObject(t *testing.T, raw json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("not an object: %s", raw)
	}
	return m
}

type priceRead struct {
	pair     string
	from, to time.Time
}

// rangeRecordingHistory records every price range the walk asks for.
type rangeRecordingHistory struct {
	*lockedHistoryReader
	mu    sync.Mutex
	reads []priceRead
}

func (r *rangeRecordingHistory) HistoryPointsInRange(ctx context.Context, p canonical.Pair, g string, from, to time.Time, limit int) ([]v1.HistoryPoint, error) {
	r.mu.Lock()
	r.reads = append(r.reads, priceRead{pair: p.String(), from: from, to: to})
	r.mu.Unlock()
	return r.lockedHistoryReader.HistoryPointsInRange(ctx, p, g, from, to, limit)
}

// The price walk spans a reserve's own data, not the requested window: a window opening years earlier reads prices
// only from six hours before the first hour with an amount to one hour past the last, and a reserve with no amount
// reads none.
func TestLendingPoolRevenue_PriceWalkIsBoundedByTheData(t *testing.T) {
	pool, xlm, idle := mkCStrkey(t, 1), canonical.NativeSACContractID(), mkCStrkey(t, 6)
	idleAsset, err := canonical.ParseAsset(idle)
	if err != nil {
		t.Fatal(err)
	}
	h := func(n int) time.Time { return time.Date(2027, 3, 1, n, 0, 0, 0, time.UTC) }
	var active []clickhouse.LendingRevenueBucket
	for _, n := range []int{1, 5} {
		s := clickhouse.NewLendingRevenueSums()
		s.Gross.SetInt64(10_000_000)
		active = append(active, clickhouse.LendingRevenueBucket{Start: h(n), Sums: s})
	}
	gulp := clickhouse.NewLendingRevenueSums()
	gulp.Gulped.SetInt64(10_000_000) // not a valued amount
	idleHours := []clickhouse.LendingRevenueBucket{{Start: h(3), Sums: gulp}}
	from := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	hist := &rangeRecordingHistory{lockedHistoryReader: xlmUSDPrices(map[time.Time]string{h(1): "1"})}
	v2 := blend.PoolV2
	srv := v1.New(v1.Options{
		Explorer: &stubRevenueExplorer{stubExplorerReader: &stubExplorerReader{}, rev: clickhouse.LendingRevenue{
			Covered: true, FloorLedger: 1, FloorTime: from.Add(-time.Hour), ThroughLedger: 50, ThroughHour: h(23), From: from, Before: h(24),
			Reserves: []clickhouse.LendingRevenueReserve{
				{Asset: xlm, Hours: active, Buckets: clickhouse.FoldLendingRevenueDays(active)},
				{Asset: idle, Hours: idleHours, Buckets: clickhouse.FoldLendingRevenueDays(idleHours)},
			},
		}},
		Lending: &stubLendingReader{version: &v2}, History: hist, TokenDecimals: &lockstepDecStub{d: 7, found: true},
	})
	rec := getLendingHistory(t, srv, "/v1/lending/pools/"+pool+"/revenue?from=2024-01-01T00:00:00Z&to=2027-03-02T00:00:00Z")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if len(hist.reads) == 0 {
		t.Fatal("no price read for the reserve with amounts")
	}
	lo, hi := h(1).Add(-6*time.Hour), h(6)
	for _, r := range hist.reads {
		if r.from.Before(lo) || r.to.After(hi) {
			t.Errorf("price read %s [%s, %s), want inside [%s, %s)", r.pair, r.from, r.to, lo, hi)
		}
		if strings.Contains(r.pair, idleAsset.String()) {
			t.Errorf("price read for the reserve with no valued amount: %s", r.pair)
		}
	}
	idleUSD := rawObject(t, reserveNamed(t, lendingUSDData(t, rec.Body.Bytes())["reserves"], idle)["usd"])
	if string(idleUSD["interest_accrued_usd"]) != `"0.00"` || string(idleUSD["lower_bound"]) != "false" {
		t.Errorf("idle reserve usd = %s / %s, want 0.00 complete", idleUSD["interest_accrued_usd"], idleUSD["lower_bound"])
	}
}

// An operator source exclusion over an hour's price leaves that hour unvalued: counted, named, and the sum a lower bound.
func TestLendingPoolRevenue_ExcludedSourceDoesNotValueHour(t *testing.T) {
	pool, xlm := mkCStrkey(t, 1), canonical.NativeSACContractID()
	h := func(n int) time.Time { return time.Date(2027, 3, 1, n, 0, 0, 0, time.UTC) }
	var hours []clickhouse.LendingRevenueBucket
	for _, n := range []int{1, 10} {
		s := clickhouse.NewLendingRevenueSums()
		s.Gross.SetInt64(10_000_000)
		s.SupplierIntended.Set(s.Gross)
		s.SupplierRealised.Set(s.Gross)
		hours = append(hours, clickhouse.LendingRevenueBucket{Start: h(n), Sums: s})
	}
	get := func(excl v1.PriceExclusionGate) (map[string]json.RawMessage, v1.Flags) {
		t.Helper()
		v2 := blend.PoolV2
		srv := v1.New(v1.Options{
			Explorer: &stubRevenueExplorer{stubExplorerReader: &stubExplorerReader{}, rev: clickhouse.LendingRevenue{
				Covered: true, FloorLedger: 1, FloorTime: h(0).Add(-time.Hour), ThroughLedger: 50, ThroughHour: h(12), From: h(0), Before: h(12),
				Reserves: []clickhouse.LendingRevenueReserve{{Asset: xlm, Buckets: hours, Hours: hours}},
			}},
			Lending: &stubLendingReader{version: &v2}, History: xlmUSDPrices(map[time.Time]string{h(1): "0.1", h(10): "0.3"}),
			Exclusions: excl,
		})
		rec := getLendingHistory(t, srv, "/v1/lending/pools/"+pool+"/revenue?interval=1h&from=2027-03-01T00:00:00Z&to=2027-03-01T12:00:00Z")
		var env struct {
			Data  map[string]json.RawMessage
			Flags v1.Flags
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &env) != nil {
			t.Fatalf("status %d: %s", rec.Code, rec.Body)
		}
		return rawObject(t, env.Data["totals_usd"]), env.Flags
	}
	if tot, _ := get(nil); string(tot["interest_accrued_usd"]) != `"0.40"` || string(tot["lower_bound"]) != "false" {
		t.Fatalf("control: totals_usd = %v; want 0.40, complete", tot)
	}
	tot, flags := get(assetExclGate(exclTable{rows: []timescale.SourceExclusion{{
		ID: 1, Source: "soroswap", Base: "native", Quote: "fiat:USD", From: h(10), Until: h(11),
	}}}))
	if string(tot["interest_accrued_usd"]) != `"0.10"` || string(tot["lower_bound"]) != "true" ||
		string(tot["excluded"]) != `[{"asset":"`+xlm+`","reason":"price_missing","count":1}]` {
		t.Errorf("totals_usd = %v; want 0.10, a lower bound naming the unpriced hour", tot)
	}
	if flags.FillsWithheldReason != v1.PriceWithheldSourceExcluded {
		t.Errorf("flags = %+v; want fills withheld for the source exclusion", flags)
	}
}

// Through the handler: XLM is valued at each hour's own price; a thin-market reserve is withheld as /history withholds
// it, named in totals_usd.excluded and left out of the pool sum, never added as zero.
func TestLendingPoolRevenue_USDAtTheHourAndThinWithheld(t *testing.T) {
	pool, xlm, thin := mkCStrkey(t, 1), canonical.NativeSACContractID(), mkCStrkey(t, 5)
	thinAsset, err := canonical.ParseAsset(thin)
	if err != nil {
		t.Fatal(err)
	}
	h := func(n int) time.Time { return time.Date(2027, 3, 1, n, 0, 0, 0, time.UTC) }
	hours := func() []clickhouse.LendingRevenueBucket {
		var out []clickhouse.LendingRevenueBucket
		for _, n := range []int{1, 2} {
			s := clickhouse.NewLendingRevenueSums()
			s.Gross.SetInt64(10_000_000) // 1 token
			s.SupplierIntended.Set(s.Gross)
			s.SupplierRealised.Set(s.Gross)
			out = append(out, clickhouse.LendingRevenueBucket{Start: h(n), Sums: s})
		}
		return out
	}
	xh, th := hours(), hours()
	ex := &stubRevenueExplorer{stubExplorerReader: &stubExplorerReader{}, rev: clickhouse.LendingRevenue{
		Covered: true, FloorLedger: 1, FloorTime: h(0).Add(-time.Hour), ThroughLedger: 50, ThroughHour: h(5), From: h(0), Before: h(4),
		Reserves: []clickhouse.LendingRevenueReserve{{Asset: xlm, Buckets: xh, Hours: xh}, {Asset: thin, Buckets: th, Hours: th}},
	}}
	prices := map[time.Time]string{h(1): "0.1", h(2): "0.3"}
	usd := canonical.Asset{Type: canonical.AssetFiat, Code: "USD"}
	v2 := blend.PoolV2
	srv := v1.New(v1.Options{
		Explorer: ex, Lending: &stubLendingReader{version: &v2},
		History: pairPrices(map[string]map[time.Time]string{
			"native/fiat:USD": prices, canonical.Pair{Base: thinAsset, Quote: usd}.String(): prices,
		}),
		TokenDecimals: &lockstepDecStub{d: 7, found: true},
		Substance:     pricingguard.NewSubstanceGate(thinStore{thin: map[string]bool{thinAsset.String(): true}}, pricingguard.SubstanceGateOptions{}),
	})
	rec := getLendingHistory(t, srv, "/v1/lending/pools/"+pool+"/revenue?interval=1h&from=2027-03-01T00:00:00Z&to=2027-03-01T04:00:00Z")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	data := lendingUSDData(t, rec.Body.Bytes())
	x, tr := rawObject(t, reserveNamed(t, data["reserves"], xlm)["usd"]), rawObject(t, reserveNamed(t, data["reserves"], thin)["usd"])
	if string(x["interest_accrued_usd"]) != `"0.40"` || string(x["priced_hours"]) != "2" || string(x["lower_bound"]) != "false" {
		t.Errorf("xlm usd = %s %s %s, want 0.40 over 2 priced hours", x["interest_accrued_usd"], x["priced_hours"], x["lower_bound"])
	}
	if string(tr["interest_accrued_usd"]) != "null" || string(tr["usd_reason"]) != `"withheld"` {
		t.Errorf("thin usd = %s/%s, want null withheld", tr["interest_accrued_usd"], tr["usd_reason"])
	}
	tot := rawObject(t, data["totals_usd"])
	if string(tot["interest_accrued_usd"]) != `"0.40"` || string(tot["lower_bound"]) != "true" ||
		string(tot["excluded"]) != `[{"asset":"`+thin+`","reason":"withheld","count":2}]` {
		t.Errorf("totals_usd = %s lower_bound %s excluded %s", tot["interest_accrued_usd"], tot["lower_bound"], tot["excluded"])
	}
}
