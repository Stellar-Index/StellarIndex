package v1

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

var allDegradedReasons = []string{
	obs.DegradedSourceStale, obs.DegradedCheckUnread, obs.DegradedSectionDropped, obs.DegradedPartialRead, obs.DegradedRefreshFailed,
	obs.DegradedRefreshOverrun, obs.DegradedRefreshRefused, obs.DegradedProvisional, obs.DegradedRequestCanceled, obs.DegradedOther,
}

// serveDegraded runs serve behind obs.HTTPMetrics as route, flags the response degraded when serve says so, and returns
// the one reason the counter recorded ("" when nothing was counted); it fails if more than one reason was counted.
func serveDegraded(t *testing.T, route string, serve func(ctx context.Context, w http.ResponseWriter) (degraded bool)) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+route, func(w http.ResponseWriter, r *http.Request) {
		d := serve(r.Context(), w)
		obs.MarkResponseFlags(w, d, d)
	})
	obs.HTTPMetrics(mux).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, route, nil))
	got, total := "", 0.0
	for _, reason := range allDegradedReasons {
		if n := testutil.ToFloat64(obs.APIDegradedResponsesTotal.WithLabelValues(route, reason)); n > 0 {
			got, total = reason, total+n
		}
	}
	if total > 1 {
		t.Fatalf("%s counted %v degraded responses, want at most 1", route, total)
	}
	return got
}

func poolsDegradedReason(t *testing.T, route string, c *CachedMarketsReader) string {
	t.Helper()
	return serveDegraded(t, route, func(ctx context.Context, _ http.ResponseWriter) bool {
		ctx, reads := withReadReport(ctx)
		_, _, _, stale, err := c.AllPoolsStale(ctx, timescale.PoolsFilter{Sources: []string{"aquarius"}}, "", 50, timescale.MarketsOrderVolume24hDesc)
		if err != nil {
			t.Fatalf("AllPoolsStale: %v", err)
		}
		if reads.degradedReason() != "" && !stale {
			t.Errorf("%s: a last-good copy was served unflagged stale", route)
		}
		return reads.degradedReason() != ""
	})
}

func warmPools(t *testing.T, c *CachedMarketsReader) {
	t.Helper()
	if _, _, err := c.AllPools(context.Background(), timescale.PoolsFilter{Sources: []string{"aquarius"}}, "", 50, timescale.MarketsOrderVolume24hDesc); err != nil {
		t.Fatalf("cold read: %v", err)
	}
	if !waitFor(2*time.Second, func() bool { return len(c.fills) == 0 }) {
		t.Fatal("cold fill did not return its slot")
	}
}

func TestPoolsDegradedReason_healthyRecordsNone(t *testing.T) {
	c := NewCachedMarketsReader(&swrPoolsUpstream{fakeMarketsReader: &fakeMarketsReader{}}, time.Minute)
	warmPools(t, c)
	if got := poolsDegradedReason(t, "/t/pools-healthy", c); got != "" {
		t.Errorf("fresh hit counted reason %q, want none", got)
	}
}

func TestPoolsDegradedReason_refreshRefusedWhenNoSlot(t *testing.T) {
	c := NewCachedMarketsReader(&swrPoolsUpstream{fakeMarketsReader: &fakeMarketsReader{}}, time.Minute)
	warmPools(t, c)
	// Within ttl+budget, so only the missing refresh makes the copy stale.
	c.mu.Lock()
	for _, e := range c.entries {
		e.at = e.at.Add(-time.Minute - time.Second)
	}
	c.mu.Unlock()
	for range marketsHeadRefreshSlots {
		c.refreshes <- struct{}{}
	}
	if got := poolsDegradedReason(t, "/t/pools-refused", c); got != obs.DegradedRefreshRefused {
		t.Errorf("reason = %q, want %q", got, obs.DegradedRefreshRefused)
	}
}

func TestPoolsDegradedReason_refreshOverrun(t *testing.T) {
	hold := newUpstreamHold(2) // the refresh parks, so it is in flight while it runs past its budget
	t.Cleanup(hold.release)
	c := NewCachedMarketsReader(&swrPoolsUpstream{fakeMarketsReader: &fakeMarketsReader{}, hold: hold}, time.Minute)
	warmPools(t, c)
	expireMarketsEntries(c)
	if got := poolsDegradedReason(t, "/t/pools-overrun-started", c); got != "" {
		t.Errorf("the ask that starts the refresh: reason = %q, want none", got)
	}
	c.mu.Lock()
	for _, e := range c.entries {
		e.flightAt = e.flightAt.Add(-marketsRefreshBudget)
	}
	c.mu.Unlock()
	if got := poolsDegradedReason(t, "/t/pools-overrun", c); got != obs.DegradedRefreshOverrun {
		t.Errorf("reason = %q, want %q", got, obs.DegradedRefreshOverrun)
	}
}

func TestPoolsDegradedReason_refreshFailed(t *testing.T) {
	up := &swrPoolsUpstream{fakeMarketsReader: &fakeMarketsReader{}}
	up.failGE2.Store(true)
	c := NewCachedMarketsReader(up, time.Minute)
	warmPools(t, c)
	expireMarketsEntries(c)
	f := timescale.PoolsFilter{Sources: []string{"aquarius"}}
	if _, _, err := c.AllPools(context.Background(), f, "", 50, timescale.MarketsOrderVolume24hDesc); err != nil {
		t.Fatal(err)
	}
	if !waitFor(2*time.Second, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		for _, e := range c.entries {
			if e.refreshFailed && e.flight == nil {
				return true
			}
		}
		return false
	}) {
		t.Fatal("the failed refresh never settled")
	}
	// Age within budget: only the failed refresh makes the copy stale.
	c.mu.Lock()
	for _, e := range c.entries {
		e.at = time.Now().Add(-time.Minute - time.Second)
	}
	c.mu.Unlock()
	if got := poolsDegradedReason(t, "/t/pools-failed", c); got != obs.DegradedRefreshFailed {
		t.Errorf("reason = %q, want %q", got, obs.DegradedRefreshFailed)
	}
}
