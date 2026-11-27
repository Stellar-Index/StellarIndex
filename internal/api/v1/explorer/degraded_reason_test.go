package explorer

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// Each way an explorer cache can serve degraded is judged by the real verdict function and written the way the handlers write it
// (fr.note, then the flags); exactly the verdict's reason may be counted, and an age-only stale serve counts none.
func TestServedFreshness_degradedReasonCounted(t *testing.T) {
	const ttl, budget = time.Minute, 3 * time.Minute
	bound := ttl + min(ttl, budget)
	old := time.Now().Add(-bound - time.Hour)

	verdicts := []struct {
		name   string
		get    func() servedFreshness
		reason string
	}{
		{"failed refresh", func() servedFreshness {
			var f perKeyFlight
			return swrFreshness(old, ttl, budget, true, &f, "k", false)
		}, obs.DegradedRefreshFailed},
		{"refused refresh", func() servedFreshness {
			var f perKeyFlight
			return swrFreshness(old, ttl, budget, false, &f, "k", true)
		}, obs.DegradedRefreshRefused},
		{"overrun refresh", func() servedFreshness {
			var f perKeyFlight
			fl, _ := f.begin("k")
			fl.started = time.Now().Add(-budget - time.Second)
			return swrFreshness(old, ttl, budget, false, &f, "k", false)
		}, obs.DegradedRefreshOverrun},
		{"provisional payload", func() servedFreshness {
			var c contractDetailCache
			return c.freshness("ev:C1", contractDetailEntry{v: provisionalTestPayload{}, cachedAt: time.Now()}, false)
		}, obs.DegradedProvisional},
		{"cold op-type panel dropped", func() servedFreshness {
			var c opTypeStatsCache
			_, _, fr := c.get()
			return fr
		}, obs.DegradedSectionDropped},
		{"age-only stale", func() servedFreshness {
			var f perKeyFlight
			return swrFreshness(old, ttl, budget, false, &f, "k", false)
		}, ""},
	}
	all := []string{obs.DegradedRefreshFailed, obs.DegradedRefreshRefused, obs.DegradedRefreshOverrun, obs.DegradedProvisional, obs.DegradedSectionDropped, obs.DegradedOther}
	for _, tc := range verdicts {
		t.Run(tc.name, func(t *testing.T) {
			route := "/t/" + strings.ReplaceAll(tc.name, " ", "-")
			mux := http.NewServeMux()
			mux.HandleFunc("GET "+route, func(w http.ResponseWriter, _ *http.Request) {
				fr := tc.get()
				fr.note(w)
				obs.MarkResponseFlags(w, fr.degraded, fr.stale)
			})
			obs.HTTPMetrics(obs.CaptureRoute(mux)).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, route, nil))

			for _, reason := range all {
				want := 0.0
				if reason == tc.reason {
					want = 1
				}
				if got := testutil.ToFloat64(obs.APIDegradedResponsesTotal.WithLabelValues(route, reason)); got != want {
					t.Errorf("reason=%s counted %v, want %v", reason, got, want)
				}
			}
		})
	}
}
