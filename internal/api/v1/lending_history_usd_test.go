package v1_test

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
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
	pts := make([]v1.HistoryPoint, 0, len(prices))
	for b, p := range prices {
		pts = append(pts, v1.HistoryPoint{Bucket: b, VWAP: p})
	}
	slices.SortFunc(pts, func(a, b v1.HistoryPoint) int { return a.Bucket.Compare(b.Bucket) })
	return &lockedHistoryReader{pairKeyedHistoryReader: &pairKeyedHistoryReader{byPair: map[string][]v1.HistoryPoint{"native/fiat:USD": pts}}}
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
		srv := v1.New(v1.Options{Explorer: ex, Lending: &stubLendingReader{version: &v2, assets: assets}, History: hist})
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
