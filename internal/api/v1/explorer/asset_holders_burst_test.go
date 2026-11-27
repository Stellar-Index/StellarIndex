package explorer

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// burstReader blocks every holders read on release and records the peak concurrency.
type burstReader struct {
	*swrReader
	release     chan struct{}
	started     chan struct{}
	inFlight    atomic.Int64
	maxInFlight atomic.Int64
}

func (r *burstReader) AssetHolders(context.Context, string, int) ([]clickhouse.AssetHolder, int64, time.Time, error) {
	cur := r.inFlight.Add(1)
	for prev := r.maxInFlight.Load(); cur > prev && !r.maxInFlight.CompareAndSwap(prev, cur); prev = r.maxInFlight.Load() {
	}
	r.started <- struct{}{}
	<-r.release
	r.inFlight.Add(-1)
	return []clickhouse.AssetHolder{{AccountID: validTestAccount, Balance: 7}}, 1, time.Time{}, nil
}

// TestAssetHoldersCached_ColdBurstDrainsNotSheds: a burst of distinct cold assets, larger than the class share (2) and the whole gate
// (8), queues for a slot rather than answering 503, and never runs more than the class share at once.
func TestAssetHoldersCached_ColdBurstDrainsNotSheds(t *testing.T) {
	h, sr := newSWRHandler()
	const n = 10 // 2 slots + 8 waiters: the bounded capacity of one client-keyed class
	br := &burstReader{swrReader: sr, release: make(chan struct{}), started: make(chan struct{}, n)}
	h.Reader = br

	errs := make([]error, n)
	var wg sync.WaitGroup
	fire := func(i int) {
		wg.Go(func() {
			_, _, _, _, errs[i] = h.assetHoldersCached(context.Background(), fmt.Sprintf("CODE%d-ISSUER", i), 10)
		})
	}
	fire(0)
	fire(1)
	<-br.started
	<-br.started
	for i := 2; i < n; i++ {
		fire(i)
	}
	close(br.release)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("cold asset %d shed under a burst within the bounded queue: %v", i, err)
		}
	}
	if got := br.maxInFlight.Load(); got > 2 {
		t.Fatalf("max concurrent holders scans = %d, want <= 2 (the asset_holders class share)", got)
	}
}

// TestAssetHoldersCached_StaleSkipsWhenSaturated: a stale entry never waits for a slot; with the class full it is served at once,
// labelled refused for that serve, and its refresh-failed memory is left alone (a skip is not a failure).
func TestAssetHoldersCached_StaleSkipsWhenSaturated(t *testing.T) {
	h, _ := newSWRHandler()
	gate := h.detachedGate()
	for range 2 {
		if !gate.TryAcquireClass("asset_holders") {
			t.Fatal("setup: could not fill the class share")
		}
	}
	h.assetHolders.put("A-I", assetHoldersEntry{cachedAt: time.Now().Add(-2 * assetHoldersTTL)})
	_, _, _, fr, err := h.assetHoldersCached(context.Background(), "A-I", 10)
	if err != nil {
		t.Fatalf("stale read errored instead of serving: %v", err)
	}
	if fr.reason != obs.DegradedRefreshRefused {
		t.Errorf("skipped serve reason = %q, want %q", fr.reason, obs.DegradedRefreshRefused)
	}
	if e, _, _ := h.assetHolders.get("A-I"); e.refreshFailed {
		t.Fatal("a skipped stale refresh marked the entry refreshFailed")
	}
}
