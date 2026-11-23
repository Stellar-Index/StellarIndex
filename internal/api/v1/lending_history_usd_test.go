package v1_test

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// perAssetHistoryExplorer serves each reserve its own history; an asset with none is ErrLendingReserveNotFound, as the lake answers.
type perAssetHistoryExplorer struct {
	*stubExplorerReader
	byAsset map[string]clickhouse.LendingReserveHistory
}

func (e *perAssetHistoryExplorer) LendingReserveHistory(_ context.Context, _, asset string, _ clickhouse.LendingHistoryQuery) (clickhouse.LendingReserveHistory, error) {
	h, ok := e.byAsset[asset]
	if !ok {
		return clickhouse.LendingReserveHistory{}, clickhouse.ErrLendingReserveNotFound
	}
	return h, nil
}

func (e *perAssetHistoryExplorer) LendingBackstopHistory(context.Context, string, bool, clickhouse.LendingHistoryQuery) (clickhouse.LendingBackstopHistory, error) {
	return clickhouse.LendingBackstopHistory{}, nil
}

func nov(d, h int) time.Time { return time.Date(2026, 11, d, h, 0, 0, 0, time.UTC) }

// reservePoint is a 7-decimal reserve's state; amounts are base units.
func reservePoint(at time.Time, supplied, borrowed int64) clickhouse.LendingReservePoint {
	return clickhouse.LendingReservePoint{CloseTime: at, StateLedger: 1, State: clickhouse.BlendReserveState{
		Decimals: 7, DecimalsFound: true,
		Data:    blend.ReserveData{BRate: big.NewInt(1e12), DRate: big.NewInt(1e12), IRMod: big.NewInt(1), BackstopCredit: big.NewInt(0), Version: blend.PoolV2},
		Metrics: blend.ReserveMetrics{SuppliedUnderlying: big.NewInt(supplied), BorrowedUnderlying: big.NewInt(borrowed)},
	}}
}

// lockedHistoryReader serialises the fixture's call log: the pool route reads its reserves' prices concurrently.
type lockedHistoryReader struct {
	*pairKeyedHistoryReader
	mu sync.Mutex
}

func (r *lockedHistoryReader) HistoryPointsInRange(ctx context.Context, p canonical.Pair, g string, from, to time.Time, limit int) ([]v1.HistoryPoint, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pairKeyedHistoryReader.HistoryPointsInRange(ctx, p, g, from, to, limit)
}

// xlmUSDPrices is a daily XLM/USD series: one VWAP per listed bucket.
func xlmUSDPrices(prices map[time.Time]string) *lockedHistoryReader {
	return pairPrices(map[string]map[time.Time]string{"native/fiat:USD": prices})
}

// pairPrices serves each pair key its listed buckets' VWAPs.
func pairPrices(byPair map[string]map[time.Time]string) *lockedHistoryReader {
	out := map[string][]v1.HistoryPoint{}
	for pair, prices := range byPair {
		pts := make([]v1.HistoryPoint, 0, len(prices))
		for b, p := range prices {
			pts = append(pts, v1.HistoryPoint{Bucket: b, VWAP: p})
		}
		slices.SortFunc(pts, func(a, b v1.HistoryPoint) int { return a.Bucket.Compare(b.Bucket) })
		out[pair] = pts
	}
	return &lockedHistoryReader{pairKeyedHistoryReader: &pairKeyedHistoryReader{byPair: out}}
}

func lendingUSDData(t *testing.T, body []byte) map[string]json.RawMessage {
	t.Helper()
	var env struct{ Data map[string]json.RawMessage }
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	return env.Data
}

func rawObjects(t *testing.T, raw json.RawMessage) []map[string]json.RawMessage {
	t.Helper()
	var out []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestLendingReserveHistory_USDAtEachBucketsOwnPrice pins the money rules on the reserve route: each point is valued at the
// USD price of its own bucket (never a later one, never today's), USD is a decimal string, and a point with no price within
// the bound is null with a named reason and counted, never 0.
func TestLendingReserveHistory_USDAtEachBucketsOwnPrice(t *testing.T) {
	pool, asset := mkCStrkey(t, 1), canonical.NativeSACContractID()
	v2 := blend.PoolV2
	ex := &perAssetHistoryExplorer{stubExplorerReader: &stubExplorerReader{}, byAsset: map[string]clickhouse.LendingReserveHistory{
		asset: {Protocol: clickhouse.LendingBlendV2, Points: []clickhouse.LendingReservePoint{
			reservePoint(time.Date(2026, 10, 20, 5, 0, 0, 0, time.UTC), 1_000_000_000, 400_000_000), // seed, in force at from
			reservePoint(nov(3, 12), 2_000_000_000, 500_000_000),
			reservePoint(nov(7, 1), 3_000_000_000, 0),
		}},
	}}
	hist := xlmUSDPrices(map[time.Time]string{nov(1, 0): "0.1", nov(3, 0): "0.2", nov(4, 0): "0.3"})
	srv := v1.New(v1.Options{Explorer: ex, Lending: &stubLendingReader{version: &v2}, History: hist})
	rec := getLendingHistory(t, srv, "/v1/lending/pools/"+pool+"/reserves/"+asset+"/history?from=2026-11-01T00:00:00Z&to=2026-11-08T00:00:00Z")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	d := lendingUSDData(t, rec.Body.Bytes())
	if string(d["usd_unvalued_points"]) != "1" || string(d["price_max_age_hours"]) != "48" {
		t.Fatalf("unvalued = %s, max age = %s", d["usd_unvalued_points"], d["price_max_age_hours"])
	}
	pts := rawObjects(t, d["points"])
	want := []struct{ supplied, borrowed, priceAt, reason string }{
		{`"10.00"`, `"4.00"`, `"2026-11-01T00:00:00Z"`, `null`},
		{`"40.00"`, `"10.00"`, `"2026-11-03T00:00:00Z"`, `null`},
		{`null`, `null`, `null`, `"no_price_within_bound"`},
	}
	for i, w := range want {
		p := pts[i]
		if string(p["supplied_usd"]) != w.supplied || string(p["borrowed_usd"]) != w.borrowed ||
			string(p["price_at"]) != w.priceAt || string(p["usd_reason"]) != w.reason {
			t.Errorf("point %d = supplied %s borrowed %s price_at %s reason %s; want %+v",
				i, p["supplied_usd"], p["borrowed_usd"], p["price_at"], p["usd_reason"], w)
		}
	}
}

// TestLendingPoolHistory_TVLPerBucketAndLowerBound pins the pool series: a reserve's carried state is valued at each bucket's
// own price; a bucket that leaves a reserve unvalued is a lower bound naming it; a pool with nothing valued is null, never 0.
func TestLendingPoolHistory_TVLPerBucketAndLowerBound(t *testing.T) {
	pool, xlm, unpriced, noHistory := mkCStrkey(t, 1), canonical.NativeSACContractID(), mkCStrkey(t, 3), mkCStrkey(t, 4)
	v2 := blend.PoolV2
	cov := clickhouse.LendingHistoryCoverage{ThroughLedger: 9, ThroughTime: nov(10, 0)}
	ex := &perAssetHistoryExplorer{stubExplorerReader: &stubExplorerReader{}, byAsset: map[string]clickhouse.LendingReserveHistory{
		xlm: {Protocol: clickhouse.LendingBlendV2, Coverage: cov, Points: []clickhouse.LendingReservePoint{
			reservePoint(time.Date(2026, 10, 20, 5, 0, 0, 0, time.UTC), 1_000_000_000, 400_000_000),
			reservePoint(nov(2, 6), 2_000_000_000, 500_000_000),
		}},
		unpriced: {Protocol: clickhouse.LendingBlendV2, Coverage: cov, Points: []clickhouse.LendingReservePoint{
			reservePoint(time.Date(2026, 10, 20, 5, 0, 0, 0, time.UTC), 7_000_000_000, 0),
		}},
	}}
	hist := xlmUSDPrices(map[time.Time]string{nov(1, 0): "0.1", nov(2, 0): "0.2", nov(3, 0): "0.3"})
	get := func(assets ...string) map[string]json.RawMessage {
		t.Helper()
		srv := v1.New(v1.Options{Explorer: ex, Lending: &stubLendingReader{version: &v2, assets: assets}, History: hist, CompletenessReader: verifiedLendingHistory()})
		rec := getLendingHistory(t, srv, "/v1/lending/pools/"+pool+"/history?from=2026-11-01T00:00:00Z&to=2026-11-04T00:00:00Z")
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body)
		}
		return lendingUSDData(t, rec.Body.Bytes())
	}

	d := get(xlm, unpriced, noHistory)
	if string(d["omitted_reserves"]) != `["`+noHistory+`"]` {
		t.Fatalf("omitted_reserves = %s", d["omitted_reserves"])
	}
	pts := rawObjects(t, d["points"])
	if len(pts) != 3 {
		t.Fatalf("got %d buckets, want 3: %s", len(pts), d["points"])
	}
	for i, tvl := range []string{`"10.00"`, `"40.00"`, `"60.00"`} {
		p := pts[i]
		if string(p["tvl_usd"]) != tvl || string(p["lower_bound"]) != "true" || string(p["unvalued_reserves"]) != `["`+unpriced+`"]` {
			t.Errorf("bucket %d = tvl %s lower_bound %s unvalued %s; want tvl %s, a lower bound naming %s",
				i, p["tvl_usd"], p["lower_bound"], p["unvalued_reserves"], tvl, unpriced)
		}
	}
	if string(pts[2]["borrowed_usd"]) != `"15.00"` {
		t.Errorf("bucket 2 borrowed_usd = %s, want \"15.00\"", pts[2]["borrowed_usd"])
	}

	full := rawObjects(t, get(xlm)["points"])
	if string(full[2]["tvl_usd"]) != `"60.00"` || string(full[2]["lower_bound"]) != "false" {
		t.Errorf("fully valued bucket = tvl %s lower_bound %s", full[2]["tvl_usd"], full[2]["lower_bound"])
	}
	none := rawObjects(t, get(unpriced)["points"])
	if string(none[0]["tvl_usd"]) != "null" {
		t.Errorf("a bucket with nothing valued must be null, got %s", none[0]["tvl_usd"])
	}
}

// TestLendingPoolHistory_UnverifiedReserveSetIsALowerBound: the pool's reserve list comes from the served tier, which names
// only reserves with a projected position; unless that projection is verified from genesis, every valued bucket is a lower
// bound with the reason named, never a total.
func TestLendingPoolHistory_UnverifiedReserveSetIsALowerBound(t *testing.T) {
	pool, xlm := mkCStrkey(t, 1), canonical.NativeSACContractID()
	v2 := blend.PoolV2
	ex := &perAssetHistoryExplorer{stubExplorerReader: &stubExplorerReader{}, byAsset: map[string]clickhouse.LendingReserveHistory{
		xlm: {
			Protocol: clickhouse.LendingBlendV2, Coverage: clickhouse.LendingHistoryCoverage{ThroughLedger: 9, ThroughTime: nov(10, 0)},
			Points: []clickhouse.LendingReservePoint{reservePoint(time.Date(2026, 10, 20, 5, 0, 0, 0, time.UTC), 1_000_000_000, 0)},
		},
	}}
	srv := v1.New(v1.Options{
		Explorer: ex, Lending: &stubLendingReader{version: &v2, assets: []string{xlm}},
		History: xlmUSDPrices(map[time.Time]string{nov(1, 0): "0.1"}),
	})
	rec := getLendingHistory(t, srv, "/v1/lending/pools/"+pool+"/history?from=2026-11-01T00:00:00Z&to=2026-11-02T00:00:00Z")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	d := lendingUSDData(t, rec.Body.Bytes())
	pts := rawObjects(t, d["points"])
	if len(pts) != 1 || string(pts[0]["tvl_usd"]) != `"10.00"` || string(pts[0]["lower_bound"]) != "true" {
		t.Fatalf("points = %s, want a 10.00 lower bound", d["points"])
	}
	if string(d["reserves_lower_bound_reason"]) != `"history_unverified"` {
		t.Errorf("reserves_lower_bound_reason = %s, want \"history_unverified\"", d["reserves_lower_bound_reason"])
	}
}

// historyFixture is a pool of the given reserves, each with one state from before the window, over a substance gate that
// measures thin as thin and the given exclusion gate.
func historyFixture(t *testing.T, byAsset map[string]clickhouse.LendingReserveHistory, assets []string, hist *lockedHistoryReader,
	thin map[string]bool, excl v1.PriceExclusionGate,
) *v1.Server {
	t.Helper()
	v2 := blend.PoolV2
	return v1.New(v1.Options{
		Explorer: &perAssetHistoryExplorer{stubExplorerReader: &stubExplorerReader{}, byAsset: byAsset},
		Lending:  &stubLendingReader{version: &v2, assets: assets}, History: hist, CompletenessReader: verifiedLendingHistory(),
		Substance:  pricingguard.NewSubstanceGate(thinStore{thin: thin}, pricingguard.SubstanceGateOptions{}),
		Exclusions: excl,
	})
}

var historySeed = time.Date(2026, 10, 20, 5, 0, 0, 0, time.UTC)

func reserveHistoryOf(points ...clickhouse.LendingReservePoint) clickhouse.LendingReserveHistory {
	return clickhouse.LendingReserveHistory{
		Protocol: clickhouse.LendingBlendV2,
		Coverage: clickhouse.LendingHistoryCoverage{ThroughLedger: 9, ThroughTime: nov(10, 0)}, Points: points,
	}
}

func reserveNamed(t *testing.T, raw json.RawMessage, asset string) map[string]json.RawMessage {
	t.Helper()
	for _, r := range rawObjects(t, raw) {
		if string(r["asset"]) == `"`+asset+`"` {
			return r
		}
	}
	t.Fatalf("reserve %s missing from %s", asset, raw)
	return nil
}

// TestLendingHistory_ThinReserveIsWithheldAsLive: a reserve whose only market is a thin book is withheld by the live reserve
// page, so history must not value it either: every point null with reason withheld, the pool a lower bound naming it.
func TestLendingHistory_ThinReserveIsWithheldAsLive(t *testing.T) {
	pool, xlm, thin := mkCStrkey(t, 1), canonical.NativeSACContractID(), mkCStrkey(t, 5)
	thinAsset, err := canonical.ParseAsset(thin)
	if err != nil {
		t.Fatal(err)
	}
	prices := map[time.Time]string{nov(1, 0): "0.1", nov(2, 0): "0.1", nov(3, 0): "0.1"}
	usd := canonical.Asset{Type: canonical.AssetFiat, Code: "USD"}
	srv := historyFixture(t, map[string]clickhouse.LendingReserveHistory{
		xlm:  reserveHistoryOf(reservePoint(historySeed, 1_000_000_000, 0)),
		thin: reserveHistoryOf(reservePoint(historySeed, 5_000_000_000, 0)),
	}, []string{xlm, thin}, pairPrices(map[string]map[time.Time]string{
		"native/fiat:USD": prices, canonical.Pair{Base: thinAsset, Quote: usd}.String(): prices,
	}), map[string]bool{thinAsset.String(): true}, nil)

	rec := getLendingHistory(t, srv, "/v1/lending/pools/"+pool+"/history?from=2026-11-01T00:00:00Z&to=2026-11-04T00:00:00Z")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	pts := rawObjects(t, lendingUSDData(t, rec.Body.Bytes())["points"])
	if len(pts) != 3 {
		t.Fatalf("got %d buckets, want 3", len(pts))
	}
	for i, p := range pts {
		r := reserveNamed(t, p["reserves"], thin)
		if string(p["tvl_usd"]) != `"10.00"` || string(p["lower_bound"]) != "true" || string(p["unvalued_reserves"]) != `["`+thin+`"]` ||
			string(r["supplied_usd"]) != "null" || string(r["usd_reason"]) != `"withheld"` {
			t.Errorf("bucket %d = tvl %s lower_bound %s unvalued %s thin %s/%s; want 10.00, a lower bound naming the withheld thin reserve",
				i, p["tvl_usd"], p["lower_bound"], p["unvalued_reserves"], r["supplied_usd"], r["usd_reason"])
		}
	}

	rec = getLendingHistory(t, srv, "/v1/lending/pools/"+pool+"/reserves/"+thin+"/history?from=2026-11-01T00:00:00Z&to=2026-11-04T00:00:00Z")
	for i, p := range rawObjects(t, lendingUSDData(t, rec.Body.Bytes())["points"]) {
		if string(p["supplied_usd"]) != "null" || string(p["usd_reason"]) != `"withheld"` {
			t.Errorf("reserve point %d = %s/%s, want null withheld", i, p["supplied_usd"], p["usd_reason"])
		}
	}
}

// TestLendingPoolHistory_ArchivedReserveIsNamed: a reserve whose state entry was archived still holds the pool's tokens, so a
// bucket after the archival names it (reason archived) and is a lower bound, never a total without it.
func TestLendingPoolHistory_ArchivedReserveIsNamed(t *testing.T) {
	pool, xlm, gone := mkCStrkey(t, 1), canonical.NativeSACContractID(), mkCStrkey(t, 6)
	srv := historyFixture(t, map[string]clickhouse.LendingReserveHistory{
		xlm: reserveHistoryOf(reservePoint(historySeed, 1_000_000_000, 0)),
		gone: reserveHistoryOf(reservePoint(historySeed, 3_000_000_000, 0),
			clickhouse.LendingReservePoint{CloseTime: nov(2, 6), StateLedger: 2, Removed: true}),
	}, []string{xlm, gone}, xlmUSDPrices(map[time.Time]string{nov(1, 0): "0.1", nov(2, 0): "0.1", nov(3, 0): "0.1"}), nil, nil)
	rec := getLendingHistory(t, srv, "/v1/lending/pools/"+pool+"/history?from=2026-11-01T00:00:00Z&to=2026-11-04T00:00:00Z")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	pts := rawObjects(t, lendingUSDData(t, rec.Body.Bytes())["points"])
	if len(pts) != 3 {
		t.Fatalf("got %d buckets, want 3", len(pts))
	}
	for _, p := range pts[1:] {
		r := reserveNamed(t, p["reserves"], gone)
		if string(p["lower_bound"]) != "true" || string(p["unvalued_reserves"]) != `["`+gone+`"]` || string(r["usd_reason"]) != `"archived"` {
			t.Errorf("bucket %s = lower_bound %s unvalued %s reason %s; want a lower bound naming the archived reserve",
				p["t"], p["lower_bound"], p["unvalued_reserves"], r["usd_reason"])
		}
	}
}

// TestLendingReserveHistory_UnreadExclusionsDegrade: an exclusion set that could not be read is no operator verdict, so the
// points say price_read_failed and the body is degraded and never cached.
func TestLendingReserveHistory_UnreadExclusionsDegrade(t *testing.T) {
	pool, xlm := mkCStrkey(t, 1), canonical.NativeSACContractID()
	srv := historyFixture(t, map[string]clickhouse.LendingReserveHistory{xlm: reserveHistoryOf(reservePoint(historySeed, 1_000_000_000, 0))},
		[]string{xlm}, xlmUSDPrices(map[time.Time]string{nov(1, 0): "0.1"}), nil, assetExclGate(exclTable{err: errors.New("exclusions down")}))
	rec := getLendingHistory(t, srv, "/v1/lending/pools/"+pool+"/reserves/"+xlm+"/history?from=2026-11-01T00:00:00Z&to=2026-11-02T00:00:00Z")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var env struct {
		Data  map[string]json.RawMessage
		Flags v1.Flags
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	pts := rawObjects(t, env.Data["points"])
	if len(pts) == 0 || string(pts[0]["usd_reason"]) != `"price_read_failed"` || !env.Flags.Stale {
		t.Fatalf("points %s stale %v; want price_read_failed and stale", env.Data["points"], env.Flags.Stale)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
}

// TestLendingHistory_HourlyPriceAgeAndCarry: an hourly series takes a price at most 6 hours old; a price more than one bucket
// old is marked carried, and the pool point names the reserve it carried.
func TestLendingHistory_HourlyPriceAgeAndCarry(t *testing.T) {
	pool, xlm := mkCStrkey(t, 1), canonical.NativeSACContractID()
	h := func(hour, minute int) time.Time { return time.Date(2026, 11, 1, hour, minute, 0, 0, time.UTC) }
	srv := historyFixture(t, map[string]clickhouse.LendingReserveHistory{xlm: reserveHistoryOf(
		reservePoint(historySeed, 1_000_000_000, 0), reservePoint(h(1, 30), 1_000_000_000, 0),
		reservePoint(h(4, 30), 1_000_000_000, 0), reservePoint(h(9, 30), 1_000_000_000, 0),
	)}, []string{xlm}, xlmUSDPrices(map[time.Time]string{h(0, 0): "0.1", h(1, 0): "0.2"}), nil, nil)
	rec := getLendingHistory(t, srv, "/v1/lending/pools/"+pool+"/reserves/"+xlm+"/history?interval=1h&from=2026-11-01T00:00:00Z&to=2026-11-01T12:00:00Z")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	d := lendingUSDData(t, rec.Body.Bytes())
	if string(d["price_max_age_hours"]) != "6" {
		t.Errorf("price_max_age_hours = %s, want 6", d["price_max_age_hours"])
	}
	pts := rawObjects(t, d["points"])
	want := []struct{ supplied, carried, reason string }{
		{`"10.00"`, "", `null`}, {`"20.00"`, "", `null`}, {`"20.00"`, "true", `null`}, {`null`, "", `"no_price_within_bound"`},
	}
	if len(pts) != len(want) {
		t.Fatalf("got %d points: %s", len(pts), d["points"])
	}
	for i, w := range want {
		if string(pts[i]["supplied_usd"]) != w.supplied || string(pts[i]["price_carried"]) != w.carried || string(pts[i]["usd_reason"]) != w.reason {
			t.Errorf("point %d = supplied %s carried %s reason %s; want %+v", i, pts[i]["supplied_usd"], pts[i]["price_carried"], pts[i]["usd_reason"], w)
		}
	}

	rec = getLendingHistory(t, srv, "/v1/lending/pools/"+pool+"/history?interval=1h&from=2026-11-01T00:00:00Z&to=2026-11-01T06:00:00Z")
	pp := rawObjects(t, lendingUSDData(t, rec.Body.Bytes())["points"])
	if len(pp) != 6 || string(pp[1]["price_carried_reserves"]) != `[]` || string(pp[4]["price_carried_reserves"]) != `["`+xlm+`"]` {
		t.Errorf("pool points = %s; want bucket 04:00 naming the carried reserve, 01:00 none", lendingUSDData(t, rec.Body.Bytes())["points"])
	}
}

// TestLendingReserveHistory_StablecoinProxyBasis: a price read from a book quoted in a stablecoin, not fiat:USD, says so.
func TestLendingReserveHistory_StablecoinProxyBasis(t *testing.T) {
	pool, xlm := mkCStrkey(t, 1), canonical.NativeSACContractID()
	srv := historyFixture(t, map[string]clickhouse.LendingReserveHistory{xlm: reserveHistoryOf(reservePoint(historySeed, 1_000_000_000, 0))},
		[]string{xlm}, pairPrices(map[string]map[time.Time]string{"native/crypto:USDT": {nov(1, 0): "0.1"}}), nil, nil)
	rec := getLendingHistory(t, srv, "/v1/lending/pools/"+pool+"/reserves/"+xlm+"/history?from=2026-11-01T00:00:00Z&to=2026-11-02T00:00:00Z")
	pts := rawObjects(t, lendingUSDData(t, rec.Body.Bytes())["points"])
	if len(pts) == 0 || string(pts[0]["supplied_usd"]) != `"10.00"` || string(pts[0]["price_basis"]) != `"stablecoin_proxy"` {
		t.Fatalf("points = %s; want 10.00 on a stablecoin_proxy basis", lendingUSDData(t, rec.Body.Bytes())["points"])
	}
}
