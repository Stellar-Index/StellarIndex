package v1_test

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"sync"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// A failed decimals read leaves a /reserves reserve unvalued as a failed read, never a cacheable answer; one that found
// no declaration is the settled, cacheable unvalued reserve.
func TestLendingPoolReserves_DecimalsReadErrorIsNotCached(t *testing.T) {
	pool, asset := mkCStrkey(t, 7), mkCStrkey(t, 44)
	for _, tc := range []struct {
		name    string
		dec     *lockstepDecStub
		noStore bool
	}{
		{"read failed", &lockstepDecStub{err: errors.New("lake read timed out")}, true},
		{"undeclared", &lockstepDecStub{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := v1.New(v1.Options{
				Explorer: &stubExplorerReader{reserves: []clickhouse.BlendReserveState{
					reserveState(pool, asset, 0, false, big.NewInt(3_000_000), big.NewInt(1_000_000)),
				}},
				Lending:       &stubLendingReader{assets: []string{asset}},
				Prices:        &stubPriceReader{snapshots: map[string]v1.PriceSnapshot{usdKey(t, asset): {Price: "2"}}},
				TokenDecimals: tc.dec,
			})
			resp := mustGet(t, httpTestServer(t, srv).URL+"/v1/lending/pools/"+pool+"/reserves")
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d", resp.StatusCode)
			}
			if got := resp.Header.Get("Cache-Control") == "no-store"; got != tc.noStore {
				t.Errorf("Cache-Control %q; want no-store=%v", resp.Header.Get("Cache-Control"), tc.noStore)
			}
		})
	}
}

// A BLNT plain-token backstop tier whose decimals read fails is a failed read (price_read_failed, no-store); one whose read
// found no declaration is decimals_unknown and cacheable.
func TestLendingPoolBackstop_DecimalsReadErrorIsDegradedNotUnknown(t *testing.T) {
	unknown := blend.PoolVersionUnknown
	for _, tc := range []struct {
		name, wantReason string
		dec              *lockstepDecStub
		noStore          bool
	}{
		{"read failed", "price_read_failed", &lockstepDecStub{err: errors.New("lake read timed out")}, true},
		{"undeclared", "decimals_unknown", &lockstepDecStub{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ex := &stubBackstopExplorer{stubExplorerReader: &stubExplorerReader{}, state: clickhouse.LendingBackstopState{
				Instance: &clickhouse.PoolInstance{Backstop: blnt.MainnetBackstop},
				Tiers: []clickhouse.BackstopTierState{
					{Tier: "FirstLoss", Asset: "Xlm", Token: mkCStrkey(t, 45), Balance: &clickhouse.BackstopBalance{Shares: big.NewInt(1e7), Tokens: big.NewInt(1e7), Q4W: big.NewInt(0)}},
				},
			}}
			srv := v1.New(v1.Options{
				Explorer: ex, TokenDecimals: tc.dec,
				Lending: &stubBLNTLendingReader{stubLendingReader: stubLendingReader{version: &unknown}, blntAssets: []string{mkCStrkey(t, 3)}},
			})
			rec := getLendingHistory(t, srv, "/v1/lending/pools/"+blnt.MainnetFixedPool+"/backstop")
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body)
			}
			var env struct{ Data v1.LendingBackstopView }
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil || len(env.Data.Tiers) != 1 {
				t.Fatalf("body %s: %v", rec.Body, err)
			}
			if tier := env.Data.Tiers[0]; tier.TokensUSD != nil || strOrNil(tier.USDReason) != tc.wantReason {
				t.Errorf("tier %+v; want null tokens_usd with %q", tier, tc.wantReason)
			}
			if got := rec.Header().Get("Cache-Control") == "no-store"; got != tc.noStore {
				t.Errorf("Cache-Control %q; want no-store=%v", rec.Header().Get("Cache-Control"), tc.noStore)
			}
		})
	}
}

// Reserve history reads the scale once: a read that fails on the first call leaves decimals null AND the points unvalued
// as a failed read, stale and not cached, never a null decimals beside exact USD from a second read.
func TestLendingReserveHistory_OneDecimalsReadBacksFieldAndUSD(t *testing.T) {
	pool, asset := mkCStrkey(t, 1), mkCStrkey(t, 46)
	tokAsset, err := canonical.ParseAsset(asset)
	if err != nil {
		t.Fatal(err)
	}
	usd := canonical.Asset{Type: canonical.AssetFiat, Code: "USD"}
	v2 := blend.PoolV2
	pt := reservePoint(nov(1, 5), 1_000_000_000, 0)
	pt.State.DecimalsFound = false // no config decimals: the scale is the lake's
	ex := &perAssetHistoryExplorer{stubExplorerReader: &stubExplorerReader{}, byAsset: map[string]clickhouse.LendingReserveHistory{
		asset: {Protocol: clickhouse.LendingBlendV2, Points: []clickhouse.LendingReservePoint{pt}},
	}}
	srv := v1.New(v1.Options{
		Explorer: ex, Lending: &stubLendingReader{version: &v2}, TokenDecimals: &failFirstDecimals{},
		History: pairPrices(map[string]map[time.Time]string{canonical.Pair{Base: tokAsset, Quote: usd}.String(): {nov(1, 0): "2"}}),
	})
	rec := getLendingHistory(t, srv, "/v1/lending/pools/"+pool+"/reserves/"+asset+"/history?from=2026-11-01T00:00:00Z&to=2026-11-02T00:00:00Z")
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
	if string(env.Data["decimals"]) != "null" || len(pts) == 0 || string(pts[0]["supplied_usd"]) != "null" ||
		string(pts[0]["usd_reason"]) != `"price_read_failed"` || !env.Flags.Stale {
		t.Fatalf("decimals %s, points %s, stale %v; want null decimals, unvalued price_read_failed, stale",
			env.Data["decimals"], env.Data["points"], env.Flags.Stale)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control %q; want no-store", cc)
	}
}

// failFirstDecimals fails its first read and answers 7 after.
type failFirstDecimals struct {
	mu    sync.Mutex
	calls int
}

func (d *failFirstDecimals) TokenDecimals(context.Context, string) (uint32, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	if d.calls == 1 {
		return 0, false, errors.New("lake read timed out")
	}
	return 7, true, nil
}

// /emissions serves an unread emission-token scale null like an undeclared one, but never cacheable.
func TestLendingPoolEmissions_DecimalsReadErrorIsNotCached(t *testing.T) {
	future := uint64(time.Now().Add(time.Hour).Unix()) //nolint:gosec // test clock
	for _, tc := range []struct {
		name    string
		dec     *lockstepDecStub
		noStore bool
	}{
		{"read failed", &lockstepDecStub{err: errors.New("lake read timed out")}, true},
		{"undeclared", &lockstepDecStub{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ex := &stubEmissionsExplorer{stubExplorerReader: &stubExplorerReader{}, state: emissionStreams(t, future)}
			srv := v1.New(v1.Options{Explorer: ex, Lending: &stubEmissionClaimReader{}, TokenDecimals: tc.dec})
			rec := getLendingHistory(t, srv, "/v1/lending/pools/"+mkCStrkey(t, 1)+"/emissions")
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body)
			}
			if got := rec.Header().Get("Cache-Control") == "no-store"; got != tc.noStore {
				t.Errorf("Cache-Control %q; want no-store=%v", rec.Header().Get("Cache-Control"), tc.noStore)
			}
		})
	}
}

// A failed decimals read values a revenue bucket as a failed read (price_read_failed, degraded so no-store); a read that
// found no declaration stays decimals_unknown and cacheable.
func TestLendingPoolRevenue_DecimalsReadErrorIsDegradedNotUnknown(t *testing.T) {
	pool, tok := mkCStrkey(t, 1), mkCStrkey(t, 7)
	tokAsset, err := canonical.ParseAsset(tok)
	if err != nil {
		t.Fatal(err)
	}
	usd := canonical.Asset{Type: canonical.AssetFiat, Code: "USD"}
	h := func(n int) time.Time { return time.Date(2027, 3, 1, n, 0, 0, 0, time.UTC) }
	s := clickhouse.NewLendingRevenueSums()
	s.Gross.SetInt64(10_000_000)
	hours := []clickhouse.LendingRevenueBucket{{Start: h(2), Sums: s}}
	from := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name       string
		dec        *lockstepDecStub
		wantReason string
		degraded   bool
	}{
		{"read failed", &lockstepDecStub{err: errors.New("lake read timed out")}, "price_read_failed", true},
		{"undeclared", &lockstepDecStub{}, "decimals_unknown", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v2 := blend.PoolV2
			srv := v1.New(v1.Options{
				Explorer: &stubRevenueExplorer{stubExplorerReader: &stubExplorerReader{}, rev: clickhouse.LendingRevenue{
					Covered: true, FloorLedger: 1, FloorTime: from.Add(-time.Hour), ThroughLedger: 50, ThroughHour: h(23), From: from, Before: h(24),
					Reserves: []clickhouse.LendingRevenueReserve{{Asset: tok, Hours: hours, Buckets: clickhouse.FoldLendingRevenueDays(hours)}},
				}},
				Lending:       &stubLendingReader{version: &v2},
				History:       pairPrices(map[string]map[time.Time]string{canonical.Pair{Base: tokAsset, Quote: usd}.String(): {h(2): "2"}}),
				TokenDecimals: tc.dec,
			})
			rec := getLendingHistory(t, srv, "/v1/lending/pools/"+pool+"/revenue?from=2024-01-01T00:00:00Z&to=2027-03-02T00:00:00Z")
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body)
			}
			reserve := reserveNamed(t, lendingUSDData(t, rec.Body.Bytes())["reserves"], tok)
			var points []struct {
				USD struct {
					USDReason *string `json:"usd_reason"`
				} `json:"usd"`
			}
			if err := json.Unmarshal(reserve["points"], &points); err != nil || len(points) == 0 {
				t.Fatalf("points %s: %v", reserve["points"], err)
			}
			if got := points[len(points)-1].USD.USDReason; got == nil || *got != tc.wantReason {
				t.Errorf("bucket usd_reason %s; want %q", strOrNil(got), tc.wantReason)
			}
			if got := string(rawObject(t, reserve["usd"])["usd_reason"]); got != `"`+tc.wantReason+`"` {
				t.Errorf("reserve usd_reason %s; want %q", got, tc.wantReason)
			}
			if noStore := rec.Header().Get("Cache-Control") == "no-store"; noStore != tc.degraded {
				t.Errorf("Cache-Control %q; want no-store=%v", rec.Header().Get("Cache-Control"), tc.degraded)
			}
			// A failed read is a degraded series, which the body states as stale.
			var env struct{ Flags v1.Flags }
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil || env.Flags.Stale != tc.degraded {
				t.Errorf("flags.stale %v (%v); want %v", env.Flags.Stale, err, tc.degraded)
			}
		})
	}
}
