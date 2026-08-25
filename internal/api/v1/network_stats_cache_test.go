package v1

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// countingNetStatsUpstream returns a fixed value and counts calls, so a
// test can prove the SWR path served a cached value without a fresh
// upstream hit on the request path.
type countingNetStatsUpstream struct {
	val   timescale.NetworkStats
	calls int
}

func (u *countingNetStatsUpstream) GetNetworkStats(_ context.Context) (timescale.NetworkStats, error) {
	u.calls++
	return u.val, nil
}

// TestCachedNetworkStatsAt_StaleServeReportsStale drives the cache into its
// SWR stale-serve branch (A') and proves GetNetworkStatsAt reports
// stale=true with the AGED observation time — the signal the handler needs
// to stamp an honest as_of instead of now(). A fresh hit reports
// stale=false. This is the cache half of the REC-05 fix (the handler half
// is TestNetworkStats_HonestStaleAsOf).
func TestCachedNetworkStatsAt_StaleServeReportsStale(t *testing.T) {
	up := &countingNetStatsUpstream{val: timescale.NetworkStats{MarketsCount24h: 7}}
	// A short TTL so we can expire it deterministically by sleeping past it.
	c := NewCachedNetworkStatsReader(up, 20*time.Millisecond)

	// (C) Cold leader: fills the cache, fresh.
	_, at0, stale0, err := c.GetNetworkStatsAt(context.Background())
	if err != nil {
		t.Fatalf("cold fetch: %v", err)
	}
	if stale0 {
		t.Fatal("cold-leader fetch reported stale=true, want false (just fetched live)")
	}
	if at0.IsZero() {
		t.Fatal("cold-leader fetch reported zero observedAt, want the fetch time")
	}

	// (A) Fresh hit: within TTL, still not stale, no new upstream call.
	callsBefore := up.calls
	_, _, staleFresh, _ := c.GetNetworkStatsAt(context.Background())
	if staleFresh {
		t.Error("fresh-hit reported stale=true, want false")
	}
	if up.calls != callsBefore {
		t.Errorf("fresh hit made %d upstream calls, want 0", up.calls-callsBefore)
	}

	// Expire the entry, then (A') stale-while-revalidate must serve the
	// prior value immediately with stale=true and the AGED observedAt.
	time.Sleep(30 * time.Millisecond)
	_, atStale, stale, _ := c.GetNetworkStatsAt(context.Background())
	if !stale {
		t.Error("expired SWR serve reported stale=false, want true")
	}
	if !atStale.Equal(at0) {
		t.Errorf("stale serve observedAt = %s, want the aged fill time %s (never now)", atStale, at0)
	}
}
