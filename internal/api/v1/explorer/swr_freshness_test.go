package explorer

import (
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// Every explorer SWR cache judges an expired serve through swrFreshness or contractDetailCache.freshness, so a new cache
// built on either inherits the rule: age past the bound is stale only; a failed, skipped or overrun refresh is degraded.
func TestSWRFreshness_AgeIsStaleFailureIsDegraded(t *testing.T) {
	const ttl, budget = time.Minute, 3 * time.Minute
	bound := ttl + min(ttl, budget)
	cases := []struct {
		name          string
		age           time.Duration
		refreshFailed bool
		flightAge     time.Duration // 0: no refresh in flight
		want          servedFreshness
	}{
		{"just past ttl, healthy refresh", ttl + time.Second, false, time.Millisecond, servedFreshness{}},
		{"past bound, refresh just started", bound + time.Hour, false, time.Millisecond, servedFreshness{stale: true}},
		{"past bound, no refresh in flight", bound + time.Hour, false, 0, servedFreshness{stale: true}},
		{"inside bound, last refresh failed", ttl + time.Second, true, 0, servedFreshness{stale: true, degraded: true, reason: obs.DegradedRefreshFailed}},
		{"past bound, last refresh failed", bound + time.Hour, true, time.Millisecond, servedFreshness{stale: true, degraded: true, reason: obs.DegradedRefreshFailed}},
		{"refresh running past its budget", bound + time.Hour, false, budget + time.Second, servedFreshness{stale: true, degraded: true, reason: obs.DegradedRefreshOverrun}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var f perKeyFlight
			if tc.flightAge > 0 {
				fl, _ := f.begin("k")
				fl.started = time.Now().Add(-tc.flightAge)
				defer f.end("k", fl, nil)
			}
			if got := swrFreshness(time.Now().Add(-tc.age), ttl, budget, tc.refreshFailed, &f, "k", false); got != tc.want {
				t.Errorf("swrFreshness = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestContractDetailFreshness_AgeIsStaleFailureIsDegraded(t *testing.T) {
	for _, key := range []string{"ev:C1", "ch:C1", positionsCacheKey + "G1", accountActivityCacheKey + "G1"} {
		bound := detailStaleAfterForKey(key)
		cases := []struct {
			name          string
			v             any
			age           time.Duration
			refreshFailed bool
			flightAge     time.Duration
			want          servedFreshness
		}{
			{"just past ttl, healthy refresh", "v", detailTTLForKey(key) + time.Second, false, time.Millisecond, servedFreshness{}},
			{"past bound, refresh just started", "v", bound + time.Minute, false, time.Millisecond, servedFreshness{stale: true}},
			{"last refresh failed", "v", detailTTLForKey(key) + time.Second, true, 0, servedFreshness{stale: true, degraded: true, reason: obs.DegradedRefreshFailed}},
			{"refresh running past its budget", "v", bound + time.Hour, false, contractDetailRefreshTimeout + time.Second, servedFreshness{stale: true, degraded: true, reason: obs.DegradedRefreshOverrun}},
			{"provisional payload", provisionalTestPayload{}, time.Second, false, 0, servedFreshness{stale: true, degraded: true, reason: obs.DegradedProvisional}},
		}
		for _, tc := range cases {
			t.Run(key+"/"+tc.name, func(t *testing.T) {
				var c contractDetailCache
				if tc.flightAge > 0 {
					fl, _ := c.flight.begin(key)
					fl.started = time.Now().Add(-tc.flightAge)
					defer c.flight.end(key, fl, nil)
				}
				e := contractDetailEntry{v: tc.v, cachedAt: time.Now().Add(-tc.age), refreshFailed: tc.refreshFailed}
				if got := c.freshness(key, e, false); got != tc.want {
					t.Errorf("freshness = %+v, want %+v", got, tc.want)
				}
			})
		}
	}
}
