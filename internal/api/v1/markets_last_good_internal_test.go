package v1

import (
	"net/http"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// serveCountingReasons serves path through h and returns its flags plus the one degraded reason counted for route, if any.
func serveCountingReasons(t *testing.T, h http.Handler, route, path string) (swrFlagsServe, string, float64) {
	t.Helper()
	before := map[string]float64{}
	for _, r := range allDegradedReasons {
		before[r] = testutil.ToFloat64(obs.APIDegradedResponsesTotal.WithLabelValues(route, r))
	}
	got := getSWRFlags(t, h, path)
	reason, counted := "", 0.0
	for _, r := range allDegradedReasons {
		if n := testutil.ToFloat64(obs.APIDegradedResponsesTotal.WithLabelValues(route, r)) - before[r]; n > 0 {
			reason, counted = r, counted+n
		}
	}
	return got, reason, counted
}

// A page past its hard bound whose refresh this ask starts is stale only: nothing has failed or overrun, so it keeps the route's
// Cache-Control and is not counted degraded. It becomes degraded, named, once that refresh runs past its budget or fails.
func TestMarketsLastGood_DegradedOnlyForFailedOrOverrunRefresh(t *testing.T) {
	const ttl = 30 * time.Second
	for _, tc := range []struct{ path, route string }{
		{"/v1/markets?include=sparkline", "/v1/markets"},
		{"/v1/markets", "/v1/markets"},
		{"/v1/pools", "/v1/pools"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			clk := &swrTestClock{t: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
			up := &gatedMarketsUpstream{}
			cache := NewCachedMarketsReader(up, ttl)
			cache.now, cache.sparks.now = clk.now, clk.now
			h := New(Options{Markets: cache}).Handler()
			want := func(step, reason string) {
				t.Helper()
				got, gotReason, counted := serveCountingReasons(t, h, tc.route, tc.path)
				wantCounted := 0.0
				if reason != "" {
					wantCounted = 1
				}
				if !got.stale || got.noStore != (reason != "") || gotReason != reason || counted != wantCounted {
					t.Errorf("%s: stale=%v no-store=%v reason=%q counted=%v, want stale, no-store=%v, %q counted %v",
						step, got.stale, got.noStore, gotReason, counted, reason != "", reason, wantCounted)
				}
			}
			getSWRFlags(t, h, tc.path)

			clk.add(2*ttl + time.Second)
			up.hold()
			want("old and idle, this ask starts the refresh", "")
			clk.add(marketsRefreshBudget)
			want("that refresh is past its budget", obs.DegradedRefreshOverrun)
			up.release(true)
			waitMarketsIdle(t, cache)

			up.hold()
			want("the last refresh failed", obs.DegradedRefreshFailed)
			up.release(false)
			waitMarketsIdle(t, cache)
		})
	}
}
