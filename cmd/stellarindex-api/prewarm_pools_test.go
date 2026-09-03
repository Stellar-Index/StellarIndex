package main

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// The /v1/pools prewarm shared one limit set with /v1/markets, so the pools
// slots that got warmed were the ones chosen for a different route. The
// explorer's /network page renders its "Top Stellar markets" panel from
// usePools(8, 'volume_24h_usd_desc') — i.e.
// GET /v1/pools?limit=8&order_by=volume_24h_usd_desc — and 8 was not in the
// set, so that slot had never been computed and the first visitor blocked on
// the full trades-hypertable scan.
//
// Measured on r1 2026-09-03 (v0.58.0), same process, back to back:
//
//	?limit=8    2.197 s   (unwarmed — what /network actually sends)
//	?limit=5    0.0018 s  (warmed)
//	?limit=25   0.0008 s  (warmed)
//	?limit=100  0.0010 s  (warmed)
//	?limit=200  0.0026 s  (warmed)
//
// The origin's own slow-request log names the shape:
//
//	{"path":"/v1/pools","latency_ms":2197.052,
//	 "query_shape":"limit=8&order_by=volume_24h_usd_desc","slow":true}

// recordingPoolsReader records the AllPools arguments it is asked for. Every
// other MarketsReader method is an unused stub — prewarmPools calls only
// AllPools, and a fake that answered more would be pretending to be a
// database.
type recordingPoolsReader struct {
	mu    sync.Mutex
	calls []poolsCall
}

type poolsCall struct {
	filter timescale.PoolsFilter
	cursor string
	limit  int
	order  timescale.MarketsOrder
}

func (r *recordingPoolsReader) AllPools(
	_ context.Context, filter timescale.PoolsFilter, cursor string,
	limit int, order timescale.MarketsOrder,
) ([]v1.Pool, string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, poolsCall{filter: filter, cursor: cursor, limit: limit, order: order})
	return []v1.Pool{}, "", nil
}

func (r *recordingPoolsReader) DistinctPairsExt(
	_ context.Context, _ string, _ int, _ timescale.MarketsOrder,
) ([]v1.Market, string, error) {
	return nil, "", nil
}

func (r *recordingPoolsReader) SourceMarkets(
	_ context.Context, _, _ string, _ int, _ timescale.MarketsOrder,
) ([]v1.Market, string, error) {
	return nil, "", nil
}

func (r *recordingPoolsReader) AssetMarkets(
	_ context.Context, _, _ string, _ int, _ timescale.MarketsOrder,
) ([]v1.Market, string, error) {
	return nil, "", nil
}

func (r *recordingPoolsReader) PairMarket(
	_ context.Context, _, _ canonical.Asset,
) (v1.Market, bool, error) {
	return v1.Market{}, false, nil
}

func (r *recordingPoolsReader) GetPairsVolumeHistory24hBatch(
	_ context.Context, _ [][2]string,
) (map[string][]timescale.PairVolumePoint, error) {
	return nil, nil
}

func (r *recordingPoolsReader) FirstTradeBatch(
	_ context.Context, _ [][2]string,
) (map[string]time.Time, error) {
	return nil, nil
}

func (r *recordingPoolsReader) seen() []poolsCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]poolsCall(nil), r.calls...)
}

func (r *recordingPoolsReader) seenLimits() []int {
	out := make([]int, 0, len(r.calls))
	for _, c := range r.seen() {
		out = append(out, c.limit)
	}
	sort.Ints(out)
	return out
}

// TestPrewarmPoolsWarmsTheLimitTheExplorerSends is the regression guard for
// #332 F4. It asserts the CORRECTED limit set actually reaches AllPools, not
// merely that some prewarm happened.
func TestPrewarmPoolsWarmsTheLimitTheExplorerSends(t *testing.T) {
	// The limit NetworkView.tsx's TopMarkets passes to usePools. Written out
	// here rather than read from poolsPrewarmLimits so the test pins the
	// CALLER's number — reading the set under test would assert nothing.
	const networkPageTopMarketsLimit = 8

	rec := &recordingPoolsReader{}
	cached := v1.NewCachedMarketsReader(rec, 2*time.Minute)

	prewarmPools(context.Background(), discardLogger(), cached)

	got := rec.seenLimits()
	want := []int{5, 8, 25, 100, 200}
	if len(got) != len(want) {
		t.Fatalf("prewarmed pool limits = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("prewarmed pool limits = %v, want %v", got, want)
		}
	}

	var found bool
	for _, l := range got {
		if l == networkPageTopMarketsLimit {
			found = true
		}
	}
	if !found {
		t.Fatalf("prewarmed pool limits %v omit %d — the /network page's Top Stellar "+
			"markets panel sends GET /v1/pools?limit=%d&order_by=volume_24h_usd_desc, so "+
			"its first visitor after every boot blocks on the full trades-hypertable scan "+
			"(measured 2.197s on r1 vs 0.0008-0.0026s on the warmed limits)",
			got, networkPageTopMarketsLimit, networkPageTopMarketsLimit)
	}
}

// TestPrewarmPoolsUsesTheHandlersExactCacheKeyArgs is the other half, and the
// half that has actually broken three times: a warmed slot only helps when
// EVERY key dimension matches what handlePools looks up. The cache key is
// newCacheKey("AllPools").strSet(Sources).str(Base).str(Quote).str(Asset)
// .str(cursor).int(limit).order(order), so a drifted Sources slice, a
// non-empty cursor or the wrong order each warm a phantom slot that costs a
// query and leaves every user request on the cold path.
//
//   - Sources: handlePools builds PoolsFilter{Sources: v1.DexSourceNames()},
//     NOT nil. Passing PoolsFilter{} warms key fragment `[]` while users land
//     on `[aquarius comet phoenix sdex soroswap]`.
//   - Order: handlePools maps ""|"volume_24h_usd_desc" to
//     MarketsOrderVolume24hDesc. Prewarming with MarketsOrderPair (0) was the
//     2026-05-09 bug — /v1/pools?source=sdex took 27s against a cache that
//     looked warm.
//   - Cursor: the warmed page is page one; a cursor makes it someone else's.
func TestPrewarmPoolsUsesTheHandlersExactCacheKeyArgs(t *testing.T) {
	rec := &recordingPoolsReader{}
	cached := v1.NewCachedMarketsReader(rec, 2*time.Minute)

	prewarmPools(context.Background(), discardLogger(), cached)

	calls := rec.seen()
	if len(calls) == 0 {
		t.Fatal("prewarmPools made no AllPools calls")
	}
	wantSources := v1.DexSourceNames()
	if len(wantSources) == 0 {
		t.Fatal("DexSourceNames() is empty — the registry has no DEX sources, so this guard would be vacuous")
	}
	for _, c := range calls {
		if c.order != timescale.MarketsOrderVolume24hDesc {
			t.Errorf("AllPools(limit=%d) prewarmed with order %v, want MarketsOrderVolume24hDesc — "+
				"handlePools's default for \"\"|\"volume_24h_usd_desc\"", c.limit, c.order)
		}
		if c.cursor != "" {
			t.Errorf("AllPools(limit=%d) prewarmed with cursor %q, want the empty first page", c.limit, c.cursor)
		}
		if c.filter.Base != "" || c.filter.Quote != "" || c.filter.Asset != "" {
			t.Errorf("AllPools(limit=%d) prewarmed with a base/quote/asset filter (%+v), want the unfiltered listing",
				c.limit, c.filter)
		}
		if len(c.filter.Sources) != len(wantSources) {
			t.Fatalf("AllPools(limit=%d) prewarmed Sources=%v, want the registry DEX set %v — "+
				"a mismatched Sources slice warms a phantom cache key",
				c.limit, c.filter.Sources, wantSources)
		}
		for i := range wantSources {
			if c.filter.Sources[i] != wantSources[i] {
				t.Fatalf("AllPools(limit=%d) prewarmed Sources=%v, want the registry DEX set %v",
					c.limit, c.filter.Sources, wantSources)
			}
		}
	}
}

// TestPoolsAndMarketsPrewarmLimitsAreIndependent pins the split itself. The
// two sets were one literal, which is how /v1/pools ended up warming limits
// picked for /v1/markets. If a later edit collapses them back, /v1/markets
// starts paying for a limit=8 slot nobody requests (the phantom-slot cost
// this file warns about twice) or /v1/pools loses limit=8 again.
func TestPoolsAndMarketsPrewarmLimitsAreIndependent(t *testing.T) {
	for _, l := range marketsPrewarmLimits {
		if l == 8 {
			t.Errorf("marketsPrewarmLimits %v contains 8 — that is the /v1/POOLS caller's limit; "+
				"warming it for /v1/markets is a phantom slot", marketsPrewarmLimits)
		}
	}
	// Both keep 100: /v1/markets' MarketsTable and /v1/pools' /dexes
	// PAGE_LIMIT, plus the OpenAPI default on each route.
	for _, set := range []struct {
		name   string
		limits []int
	}{{"marketsPrewarmLimits", marketsPrewarmLimits}, {"poolsPrewarmLimits", poolsPrewarmLimits}} {
		var has100 bool
		for _, l := range set.limits {
			if l == 100 {
				has100 = true
			}
		}
		if !has100 {
			t.Errorf("%s %v omits 100 — the OpenAPI default and the value a bare "+
				"listing request falls back to", set.name, set.limits)
		}
	}
}
