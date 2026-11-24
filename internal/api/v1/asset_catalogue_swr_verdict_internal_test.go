package v1

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// The catalogue's row entries report a stale read only as a last-good copy: just past the TTL with the refresh on time is
// clean; after a failed refresh, or past TTL plus one refresh budget, it is stale; a successful refresh clears the failure.
func TestCachedAssetsReaderRows_StaleFollowsTheSWRVerdict(t *testing.T) {
	const ttl, key = 2 * time.Minute, "k"
	c := NewCachedAssetsReader(nil, ttl)
	c.entries[key] = &assetsCacheEntry{at: time.Now(), rows: []timescale.AssetRow{{}}}
	fail := errors.New("down")
	var upstreamErr error
	release := make(chan struct{})
	upstream := func(context.Context) ([]timescale.AssetRow, error) {
		<-release
		return []timescale.AssetRow{{}}, upstreamErr
	}
	read := func(age time.Duration) bool {
		t.Helper()
		c.mu.Lock()
		c.entries[key].at = time.Now().Add(-age)
		c.mu.Unlock()
		ctx, rep := withReadReport(context.Background())
		if _, err := c.fetchRows(ctx, "list_coins", key, upstream); err != nil {
			t.Fatal(err)
		}
		return rep.stale.Load()
	}
	settle := func() {
		release <- struct{}{}
		for {
			c.mu.Lock()
			busy := c.entries[key].flight != nil
			c.mu.Unlock()
			if !busy {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}
	justPast := ttl + assetsRefreshBudget/2

	if read(justPast) {
		t.Error("just past ttl, refresh on time: reported stale")
	}
	upstreamErr = fail
	settle()
	if !read(justPast) {
		t.Error("previous refresh failed, retry running: not reported stale")
	}
	upstreamErr = nil
	settle()
	if read(justPast) {
		t.Error("after a successful refresh: reported stale")
	}
	settle()
	if !read(ttl + assetsRefreshBudget + time.Second) {
		t.Error("past the hard bound: not reported stale")
	}
	settle()
}

// The single-value path reports the swrCache verdict: a failed refresh flags the next read just past the TTL.
func TestCachedAssetsReaderSingle_FailedRefreshIsReported(t *testing.T) {
	const ttl = 2 * time.Minute
	c := NewCachedAssetsReader(nil, ttl)
	var now atomic.Int64
	now.Store(time.Now().UnixNano())
	c.single.now = func() time.Time { return time.Unix(0, now.Load()) }
	var calls atomic.Int32
	upstream := func(context.Context) (int64, error) {
		if calls.Add(1) == 2 {
			return 0, errors.New("down")
		}
		return 1, nil
	}
	read := func() bool {
		t.Helper()
		ctx, rep := withReadReport(context.Background())
		if _, err := swr(ctx, c, "coin_markets_count", "k", upstream); err != nil {
			t.Fatal(err)
		}
		return rep.stale.Load()
	}
	read() // cold fill
	now.Add(int64(ttl + time.Second))
	if read() {
		t.Error("just past ttl, refresh on time: reported stale")
	}
	for busy := true; busy; time.Sleep(time.Millisecond) {
		c.single.mu.Lock()
		busy = c.single.entries["k"].flight != nil
		c.single.mu.Unlock()
	}
	if !read() {
		t.Error("previous refresh failed, retry running: not reported stale")
	}
}
