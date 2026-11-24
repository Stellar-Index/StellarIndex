package v1

import (
	"testing"
	"time"
)

// A listing page past its TTL is flagged stale only as a last-good copy: after a failed refresh, or past TTL plus one
// refresh budget; a refresh that lands clears the failure.
func TestAssetList_StaleFollowsTheSWRVerdict(t *testing.T) {
	const target = "/v1/assets?limit=100"
	srv, _, clock := newCountingListingServer(t, &countingDirectory{})
	getAssetList(t, srv, target)
	markFailed := func() {
		srv.assetListCache.mu.Lock()
		defer srv.assetListCache.mu.Unlock()
		for _, e := range srv.assetListCache.entries {
			e.refreshFailed = true
		}
	}

	clock.advance(assetListCacheTTL + time.Second)
	markFailed()
	if got := getAssetList(t, srv, target); !got.stale || got.cache != "HIT" {
		t.Errorf("previous refresh failed, retry running: cache=%q stale=%v, want a stale-flagged HIT", got.cache, got.stale)
	}
	waitAssetListSettled(t, srv)

	clock.advance(assetListCacheTTL + time.Second)
	if got := getAssetList(t, srv, target); got.stale {
		t.Error("after a successful refresh: a page just past its TTL is flagged stale")
	}
	waitAssetListSettled(t, srv)

	clock.advance(assetListCacheTTL + min(assetListCacheTTL, cacheFillBudget) + time.Second)
	if got := getAssetList(t, srv, target); !got.stale || got.cache != "HIT" {
		t.Errorf("past the hard bound: cache=%q stale=%v, want a stale-flagged HIT", got.cache, got.stale)
	}
	waitAssetListSettled(t, srv)
}

// A refresh that built nothing cacheable (failed read, panic) is remembered as failed.
func TestAssetListCache_UncachedRefreshIsAFailure(t *testing.T) {
	c := newAssetListResponseCache(assetListCacheTTL, assetListCacheMaxAge)
	if err := c.put("k", Envelope{}); err != nil {
		t.Fatal(err)
	}
	c.settleFlight("k")
	if !c.entries["k"].refreshFailed {
		t.Error("settleFlight left no failure behind")
	}
	if err := c.put("k", Envelope{}); err != nil {
		t.Fatal(err)
	}
	if c.entries["k"].refreshFailed {
		t.Error("a successful put kept the failure")
	}
}
