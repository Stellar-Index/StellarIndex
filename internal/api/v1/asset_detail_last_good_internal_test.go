package v1

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// settleSWRFlightsExcept waits for every single-value refresh in flight but key's, which the test holds.
func settleSWRFlightsExcept(c *CachedAssetsReader, key string) {
	c.single.mu.Lock()
	var flights []chan struct{}
	for k, e := range c.single.entries {
		if e.flight != nil && k != key {
			flights = append(flights, e.flight)
		}
	}
	c.single.mu.Unlock()
	for _, f := range flights {
		<-f
	}
}

// countedDetailGET is clientDetailGET plus the one degraded reason it counted, if any.
func countedDetailGET(t *testing.T, s *Server) (detailGETResult, string, float64) {
	t.Helper()
	const route = "/v1/assets/{asset_id}"
	before := map[string]float64{}
	for _, r := range allDegradedReasons {
		before[r] = testutil.ToFloat64(obs.APIDegradedResponsesTotal.WithLabelValues(route, r))
	}
	got := clientDetailGET(t, s)
	reason, counted := "", 0.0
	for _, r := range allDegradedReasons {
		if n := testutil.ToFloat64(obs.APIDegradedResponsesTotal.WithLabelValues(route, r)) - before[r]; n > 0 {
			reason, counted = r, counted+n
		}
	}
	return got, reason, counted
}

// A catalogue value past its bound whose refresh this ask starts makes the detail stale only, cacheable and uncounted; the
// detail is degraded, named, once that refresh overruns its budget or fails.
func TestAssetDetail_CatalogueLastGoodOnlyDegradesForFailedOrOverrunRefresh(t *testing.T) {
	s, c, up, now := newDetailTTLServer(t)
	want := func(step, reason string) {
		t.Helper()
		got, gotReason, counted := countedDetailGET(t, s)
		wantCounted := 0.0
		if reason != "" {
			wantCounted = 1
		}
		if !got.flags.Stale || got.noStore != (reason != "") || gotReason != reason || counted != wantCounted {
			t.Errorf("%s: stale=%v no-store=%v reason=%q counted=%v, want stale, no-store=%v, %q counted %v",
				step, got.flags.Stale, got.noStore, gotReason, counted, reason != "", reason, wantCounted)
		}
	}

	gate := up.hold()
	pastStaleBound(now)
	want("old and idle, this ask starts the refresh", "")
	settleSWRFlightsExcept(c, "GetNativeAssetRow")

	now.Add(int64(cacheFillBudget))
	want("that refresh is past its budget", obs.DegradedRefreshOverrun)
	up.fail.Store(true)
	close(gate)
	settleSWRFlights(c)

	gate = up.hold()
	now.Add(int64(assetDetailDegradedTTL + time.Second))
	want("the last refresh failed", obs.DegradedRefreshFailed)
	up.fail.Store(false)
	close(gate)
	settleSWRFlights(c)
}
