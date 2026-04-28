package divergence_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/RatesEngine/rates-engine/internal/cachekeys"
	"github.com/RatesEngine/rates-engine/internal/canonical"
	"github.com/RatesEngine/rates-engine/internal/divergence"
)

// newTestService wires a Service against an in-memory miniredis +
// the supplied references. Returns the service, the redis client
// (for direct assertions), and the miniredis handle.
func newTestService(t *testing.T, refs []divergence.Reference, opts divergence.ServiceOptions) (*divergence.Service, *redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	opts.References = refs
	opts.Cache = rdb
	svc, err := divergence.NewService(opts)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc, rdb, mr
}

// TestNewService_RequiresCache — operator misconfig that omits the
// cache should fail loudly at construction, not silently skip writes.
func TestNewService_RequiresCache(t *testing.T) {
	_, err := divergence.NewService(divergence.ServiceOptions{
		References: []divergence.Reference{&stubReference{name: "a", price: 1}},
	})
	if err == nil {
		t.Fatal("expected error when Cache is nil")
	}
}

// TestRefreshPair_NoReferencesIsNoop — empty References list yields
// no Redis writes and no error.
func TestRefreshPair_NoReferencesIsNoop(t *testing.T) {
	svc, _, mr := newTestService(t, nil, divergence.ServiceOptions{})
	if err := svc.RefreshPair(context.Background(), xlmUSD(t), 1.00, time.Now()); err != nil {
		t.Errorf("RefreshPair on empty refs: %v", err)
	}
	if keys := mr.Keys(); len(keys) != 0 {
		t.Errorf("no-op should not write redis; got keys %v", keys)
	}
}

// TestRefreshPair_HappyPath — references agree with our value;
// CachedResult writes to Redis, WarningFired=false.
func TestRefreshPair_HappyPath(t *testing.T) {
	refs := []divergence.Reference{
		&stubReference{name: "a", price: 1.00},
		&stubReference{name: "b", price: 1.00},
		&stubReference{name: "c", price: 1.00},
	}
	svc, rdb, _ := newTestService(t, refs, divergence.ServiceOptions{})

	if err := svc.RefreshPair(context.Background(), xlmUSD(t), 1.00, time.Now()); err != nil {
		t.Fatalf("RefreshPair: %v", err)
	}

	body, err := rdb.Get(context.Background(), cachekeys.Divergence(canonical.NativeAsset())).Bytes()
	if err != nil {
		t.Fatalf("redis get: %v", err)
	}
	var cached divergence.CachedResult
	if err := json.Unmarshal(body, &cached); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cached.WarningFired {
		t.Errorf("WarningFired = true on consensus, want false")
	}
	if cached.SuccessCount != 3 {
		t.Errorf("SuccessCount = %d, want 3", cached.SuccessCount)
	}
	if cached.DivergencePct > 0.001 {
		t.Errorf("DivergencePct = %g, want ~0", cached.DivergencePct)
	}
}

// TestRefreshPair_FiresWarning — references agree on a price that
// disagrees with our value by > threshold; WarningFired=true.
func TestRefreshPair_FiresWarning(t *testing.T) {
	refs := []divergence.Reference{
		&stubReference{name: "a", price: 1.00},
		&stubReference{name: "b", price: 1.00},
		&stubReference{name: "c", price: 1.00},
	}
	svc, rdb, _ := newTestService(t, refs, divergence.ServiceOptions{
		Threshold:            5.0, // 5% threshold
		MinSourcesForWarning: 2,
	})

	// Our price is 10% above the consensus.
	if err := svc.RefreshPair(context.Background(), xlmUSD(t), 1.10, time.Now()); err != nil {
		t.Fatalf("RefreshPair: %v", err)
	}

	body, err := rdb.Get(context.Background(), cachekeys.Divergence(canonical.NativeAsset())).Bytes()
	if err != nil {
		t.Fatalf("redis get: %v", err)
	}
	var cached divergence.CachedResult
	_ = json.Unmarshal(body, &cached)
	if !cached.WarningFired {
		t.Errorf("WarningFired = false on 10%% deviation, want true")
	}
}

// TestRefreshPair_BelowMinSourcesNoWarning — even when divergence
// is huge, fewer than MinSourcesForWarning successful references
// suppresses the warning. Single-source disagreement shouldn't fire.
func TestRefreshPair_BelowMinSourcesNoWarning(t *testing.T) {
	refs := []divergence.Reference{
		&stubReference{name: "only", price: 1.00},
	}
	svc, rdb, _ := newTestService(t, refs, divergence.ServiceOptions{
		Threshold:            5.0,
		MinSourcesForWarning: 2, // require 2+ agreeing sources
	})
	if err := svc.RefreshPair(context.Background(), xlmUSD(t), 1.50, time.Now()); err != nil {
		t.Fatalf("RefreshPair: %v", err)
	}
	body, _ := rdb.Get(context.Background(), cachekeys.Divergence(canonical.NativeAsset())).Bytes()
	var cached divergence.CachedResult
	_ = json.Unmarshal(body, &cached)
	if cached.WarningFired {
		t.Errorf("WarningFired = true with single source; should require ≥ 2")
	}
	// But the comparator's data should still be cached so operators
	// can see what one source thinks.
	if cached.SuccessCount != 1 {
		t.Errorf("SuccessCount = %d, want 1", cached.SuccessCount)
	}
}

// TestRefreshPair_TTLApplied — Redis TTL on the cache entry matches
// cachekeys.DivergenceTTL.
func TestRefreshPair_TTLApplied(t *testing.T) {
	refs := []divergence.Reference{&stubReference{name: "a", price: 1.00}}
	svc, _, mr := newTestService(t, refs, divergence.ServiceOptions{})

	if err := svc.RefreshPair(context.Background(), xlmUSD(t), 1.00, time.Now()); err != nil {
		t.Fatalf("RefreshPair: %v", err)
	}
	ttl := mr.TTL(cachekeys.Divergence(canonical.NativeAsset()))
	if ttl == 0 || ttl > cachekeys.DivergenceTTL {
		t.Errorf("TTL = %v, want ≤ %v and > 0", ttl, cachekeys.DivergenceTTL)
	}
}

// TestLookupCached_PresentEntry — RefreshPair → LookupCached round
// trips the entry preserving every field.
func TestLookupCached_PresentEntry(t *testing.T) {
	refs := []divergence.Reference{
		&stubReference{name: "a", price: 1.05},
		&stubReference{name: "b", price: 1.05},
	}
	svc, _, _ := newTestService(t, refs, divergence.ServiceOptions{Threshold: 1.0, MinSourcesForWarning: 2})

	pair := xlmUSD(t)
	if err := svc.RefreshPair(context.Background(), pair, 1.00, time.Now()); err != nil {
		t.Fatalf("RefreshPair: %v", err)
	}

	cached, found, err := svc.LookupCached(context.Background(), pair.Base)
	if err != nil {
		t.Fatalf("LookupCached: %v", err)
	}
	if !found {
		t.Fatal("LookupCached returned found=false on a freshly-cached entry")
	}
	if cached.PairID != pair.String() {
		t.Errorf("PairID = %q, want %q", cached.PairID, pair.String())
	}
	if cached.SuccessCount != 2 {
		t.Errorf("SuccessCount = %d, want 2", cached.SuccessCount)
	}
	// 1.05 vs 1.00 = ~4.76% deviation. Threshold 1.0% → warning fires.
	if !cached.WarningFired {
		t.Errorf("WarningFired = false, expected true (4.76%% > 1%% threshold)")
	}
}

// TestLookupCached_AbsentEntry — querying an asset with no cached
// result returns (zero, false, nil) — not an error.
func TestLookupCached_AbsentEntry(t *testing.T) {
	svc, _, _ := newTestService(t, nil, divergence.ServiceOptions{})
	_, found, err := svc.LookupCached(context.Background(), canonical.NativeAsset())
	if err != nil {
		t.Errorf("LookupCached on absent entry: %v", err)
	}
	if found {
		t.Errorf("found = true on absent entry")
	}
}

// TestRefreshPair_DefaultsApplied — zero-value options use sensible
// defaults: 5% threshold, 2 min-sources, 5s timeout.
func TestRefreshPair_DefaultsApplied(t *testing.T) {
	refs := []divergence.Reference{
		&stubReference{name: "a", price: 1.00},
		&stubReference{name: "b", price: 1.00},
		&stubReference{name: "c", price: 1.00},
	}
	// Zero-value options: defaults should kick in (5% threshold,
	// 2 min sources). 4% deviation → no warning.
	svc, rdb, _ := newTestService(t, refs, divergence.ServiceOptions{})
	if err := svc.RefreshPair(context.Background(), xlmUSD(t), 1.04, time.Now()); err != nil {
		t.Fatalf("RefreshPair: %v", err)
	}
	body, _ := rdb.Get(context.Background(), cachekeys.Divergence(canonical.NativeAsset())).Bytes()
	var cached divergence.CachedResult
	_ = json.Unmarshal(body, &cached)
	if cached.WarningFired {
		t.Errorf("4%% deviation should not fire under default 5%% threshold")
	}

	// 6% deviation → warning fires.
	if err := svc.RefreshPair(context.Background(), xlmUSD(t), 1.06, time.Now()); err != nil {
		t.Fatalf("RefreshPair: %v", err)
	}
	body, _ = rdb.Get(context.Background(), cachekeys.Divergence(canonical.NativeAsset())).Bytes()
	_ = json.Unmarshal(body, &cached)
	if !cached.WarningFired {
		t.Errorf("6%% deviation should fire under default 5%% threshold")
	}
}
