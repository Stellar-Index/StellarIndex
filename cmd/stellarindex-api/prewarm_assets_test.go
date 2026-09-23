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
// always returns empty — used by tests unrelated to T279 so they keep
// exercising exactly the call set they were written against.
const noCatalogueFillTestLen = 1 << 20

// The /v1/assets listing handler overfetches by one —
// `ListAssetsOptions{Limit: limit + 1}` — so a `?limit=50` request looks
// up the cache key for 51. The prewarm never mirrored that, so the route
// had no warm key of its own.
//
// Measured at the r1 origin on 2026-09-01, with ?limit=50 kept warm by
// live traffic and ?limit=51 not:
//
//	?limit=50  → internal Limit 51   0.007 s
//	?limit=51  → internal Limit 52   1.359 s
//
// Two adjacent user-facing limits, 180x apart. Whether a request is fast
// depends entirely on whether its +1 key happens to be warm, which is
// why the arithmetic is done in exactly one place and asserted here.
//
// The limit SET is equally load-bearing and is drawn from observed
// traffic, not from reading the frontend. An earlier revision inferred
// 1/10/100/500 from the explorer source and missed `?limit=50` — which
// production then showed to be the single most expensive shape on the
// API (18 requests, 4053 ms average, 72.9 s of slow time in 100
// minutes). Guessing the key set is how a prewarm becomes a phantom
// slot: it costs a query every cycle and leaves the real caller cold.

// TestPrewarmAssetListingsMirrorsTheHandlerOverfetch is the guard that
// matters. Warming `Limit: userLimit` instead of `userLimit + 1` warms a
// key no request ever looks up — the failure is completely silent, since
// the cache reports healthy and every user request still pays the cold
// read.
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

// TestCatalogueFillPrewarmOptionsMirrorsTheUnifiedHandler is the T279
// guard: the unified (asset_class=all) landing page's catalogue phase
// (serveCatalogueUnifiedPage) fills any shortfall below the user's limit
// from the classic phase with a limit of (userLimit-catalogueLen)+
// AssetsListOverfetchBy, Order=Volume24hUSDDesc — NOT the userLimit+1 key
// assetListingPrewarmOptions warms. On the real catalogue (~45 Stellar-
// issued rows), only userLimit 50/100/500 overrun it (5/55/455 remaining);
// 1/5/10 never reach the classic phase at all. Pre-fix, none of these
// catalogue-fill keys were warmed, so the explorer's default ?limit=100
// landing-page load (remaining 55, Limit 56) paid the cold read on every
// restart.
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

	// userLimit 50 → remaining 5 → Limit 6.
	// userLimit 100 (the explorer's default landing-page load) →
	//   remaining 55 → Limit 56.
	// userLimit 500 → remaining 455 → Limit 456.
	want := map[int]bool{6: true, 56: true, 456: true}
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
	// userLimit 1/5/10 never reach the classic phase (the catalogue alone
	// covers them) — Limit 2/6/11 (the DIRECT handler's own overfetch
	// keys, from a different function) must not appear here.
	for _, l := range []int{2, 11} {
		if got[l] {
			t.Fatalf("catalogueFillPrewarmOptions(%d) = %v warms Limit %d, "+
				"but the catalogue alone satisfies that userLimit — the classic "+
				"phase is never reached, so this is a phantom slot", catalogueLen, opts, l)
		}
	}
}
