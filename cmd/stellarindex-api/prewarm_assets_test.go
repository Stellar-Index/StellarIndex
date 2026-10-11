package main

import (
	"context"
	"sort"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// noCatalogueFillTestLen is a catalogueLen so far above any real
// assetListingPrewarmLimits value (max 500) that catalogueFillPrewarmOptions
// always returns empty — used by tests unrelated to catalogue fill so they keep
// exercising exactly the call set they were written against.
const noCatalogueFillTestLen = 1 << 20

// The /v1/assets handler overfetches by one (Limit: limit + 1), so a ?limit=50 request looks up
// the cache key for 51; the +1 arithmetic lives in one place and is asserted here. The limit set
// is drawn from observed traffic, since a guessed key set becomes a phantom slot that costs a
// query every cycle and leaves the real caller cold.

// TestPrewarmAssetListingsMirrorsTheHandlerOverfetch pins warming userLimit + 1. Warming
// userLimit warms a key no request looks up, silently, while every user request pays the cold read.
func TestPrewarmAssetListingsMirrorsTheHandlerOverfetch(t *testing.T) {
	opts := assetListingPrewarmOptions()
	limitsFor := func(order timescale.AssetsOrder) []int {
		var out []int
		for _, o := range opts {
			if o.Order == order {
				out = append(out, o.Limit)
			}
		}
		sort.Ints(out)
		return out
	}

	for _, order := range []timescale.AssetsOrder{
		timescale.AssetsOrderObservationCountDesc,
		timescale.AssetsOrderVolume24hUSDDesc,
	} {
		got := limitsFor(order)
		want := make([]int, 0, len(assetListingPrewarmLimits))
		for _, l := range assetListingPrewarmLimits {
			want = append(want, l+v1.AssetsListOverfetchBy)
		}
		sort.Ints(want)

		if len(got) != len(want) {
			t.Fatalf("order %v: warmed %v, want %v", order, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("order %v: warmed %v, want %v — the handler overfetches by "+
					"one, so warming the bare user limit warms a key nothing looks up",
					order, got, want)
			}
		}
	}
}

// TestPrewarmAssetListingsCoversTheObservedHotShapes pins the two shapes
// that production showed to be 84% of all slow time. If either drops out
// of the set, the most expensive requests on the API go cold again on
// every cache expiry.
func TestPrewarmAssetListingsCoversTheObservedHotShapes(t *testing.T) {
	has := func(l int) bool {
		for _, x := range assetListingPrewarmLimits {
			if x == l {
				return true
			}
		}
		return false
	}
	// ?limit=50 — 18 requests, 4053 ms avg, 72.9 s of slow time.
	if !has(50) {
		t.Errorf("assetListingPrewarmLimits %v omits 50 — the most expensive single "+
			"shape observed in production", assetListingPrewarmLimits)
	}
	// ?include=sparkline&limit=10&order_by=volume_24h_usd_desc —
	// 16 requests, 4485 ms avg, 71.8 s. The include costs nothing
	// measurably; the limit and the order are what select the key.
	if !has(10) {
		t.Errorf("assetListingPrewarmLimits %v omits 10 — the explorer home table's "+
			"limit", assetListingPrewarmLimits)
	}
}

// TestPrewarmAssetListingsNilReaderIsSafe — the prewarm goroutine runs
// detached, so neither a nil reader nor an absent snapshot store (Redis
// unconfigured) may take the whole cadence down.
func TestPrewarmAssetListingsNilReaderIsSafe(t *testing.T) {
	prewarmAssetListings(context.Background(), discardLogger(), nil, nil, 45)
}

// TestCatalogueFillPrewarmOptionsMirrorsTheUnifiedHandler pins the key the unified landing page's
// catalogue phase uses for its shortfall: limit (userLimit-catalogueLen)+AssetsListOverfetchBy
// with Order=Volume24hUSDDesc, not the userLimit+1 key. On ~45 catalogue rows only userLimit
// 50/100/500 overrun it; 1/5/10 never reach the classic phase.
func TestCatalogueFillPrewarmOptionsMirrorsTheUnifiedHandler(t *testing.T) {
	const catalogueLen = 45 // matches serveCatalogueUnifiedPage's own doc comment
	opts := catalogueFillPrewarmOptions(catalogueLen)

	got := map[int]bool{}
	for _, o := range opts {
		if o.Order != timescale.AssetsOrderVolume24hUSDDesc {
			t.Fatalf("catalogueFillPrewarmOptions returned order %v, want %v — "+
				"fetchClassicUnifiedRows never varies the classic-phase order",
				o.Order, timescale.AssetsOrderVolume24hUSDDesc)
		}
		got[o.Limit] = true
	}

	// Expected keys are derived from the production limit set and handler
	// overfetch, so a change to either moves the guard with it.
	want := map[int]bool{}
	for _, userLimit := range assetListingPrewarmLimits {
		if remaining := userLimit - catalogueLen; remaining > 0 {
			want[remaining+v1.AssetsListOverfetchBy] = true
		}
	}
	if len(want) == 0 {
		t.Fatalf("catalogueLen %d outgrew every prewarm limit %v; the guard checks nothing",
			catalogueLen, assetListingPrewarmLimits)
	}
	if len(got) != len(want) {
		t.Fatalf("catalogueFillPrewarmOptions(%d) = %v, want exactly Limits %v",
			catalogueLen, opts, want)
	}
	for l := range want {
		if !got[l] {
			t.Fatalf("catalogueFillPrewarmOptions(%d) = %v, missing Limit %d",
				catalogueLen, opts, l)
		}
	}
	// A userLimit the catalogue alone covers never reaches the classic
	// phase — the DIRECT handler's own userLimit+overfetch key (from a
	// different function) must not appear here.
	for _, userLimit := range assetListingPrewarmLimits {
		if userLimit > catalogueLen {
			continue
		}
		if l := userLimit + v1.AssetsListOverfetchBy; got[l] && !want[l] {
			t.Fatalf("catalogueFillPrewarmOptions(%d) = %v warms Limit %d, "+
				"but the catalogue alone satisfies that userLimit — the classic "+
				"phase is never reached, so this is a phantom slot", catalogueLen, opts, l)
		}
	}
}
