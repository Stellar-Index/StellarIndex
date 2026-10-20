package main

import (
	"context"
	"sync"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// recordingPoolsReader records the AllPools arguments asked for; other MarketsReader methods are unused stubs, since
// prewarmPools calls only AllPools and a fake that answered more would pretend to be a database.
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

// TestPrewarmPoolsWarmsTheLimitsTheExplorerSends: every limit a caller sends must be a hit after one prewarm. The limits
// are written out, not derived, so the test pins the callers' numbers: 8 is the /network top-markets panel, 100 the
// OpenAPI default and the /dexes page.
func TestPrewarmPoolsWarmsTheLimitsTheExplorerSends(t *testing.T) {
	rec := &recordingPoolsReader{}
	cached := v1.NewCachedMarketsReader(rec, 2*time.Minute)

	prewarmPools(context.Background(), discardLogger(), cached)
	warmed := len(rec.seen())
	if warmed != 1 {
		t.Fatalf("prewarmPools made %d AllPools scans, want 1 head-slot scan", warmed)
	}
	filter := timescale.PoolsFilter{Sources: v1.DexSourceNames()}
	for _, limit := range []int{5, 8, 25, 100, 200} {
		if _, _, err := cached.AllPools(context.Background(), filter, "", limit, timescale.MarketsOrderVolume24hDesc); err != nil {
			t.Fatal(err)
		}
		if n := len(rec.seen()); n != warmed {
			t.Fatalf("GET /v1/pools?limit=%d after prewarm reached the database (%d scans); its first visitor pays the cold scan", limit, n)
		}
	}
}

// TestPrewarmPoolsUsesTheHandlersExactCacheKeyArgs: every cache key dimension must match handlePools or the warmed slot
// is a phantom. Sources must be v1.DexSourceNames(), not nil; order MarketsOrderVolume24hDesc, not MarketsOrderPair (0);
// cursor empty (page one).
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
