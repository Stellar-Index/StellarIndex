package v1_test

import (
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
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
		})
	}
}
