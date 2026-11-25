package obs

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestDegradedResponses_countedByReason(t *testing.T) {
	cases := []struct {
		name     string
		mark     func(w http.ResponseWriter)
		route    string
		reason   string // "" = nothing may be counted
		wantSeen string
	}{
		{"named reason", func(w http.ResponseWriter) {
			MarkDegradedReason(w, DegradedRefreshOverrun)
			MarkResponseFlags(w, true, true)
		}, "/t/named", DegradedRefreshOverrun, DegradedRefreshOverrun},
		{"first reason wins", func(w http.ResponseWriter) {
			MarkDegradedReason(w, DegradedRefreshFailed)
			MarkDegradedReason(w, DegradedProvisional)
			MarkResponseFlags(w, true, true)
		}, "/t/first", DegradedRefreshFailed, DegradedRefreshFailed},
		{"degraded without a reason", func(w http.ResponseWriter) {
			MarkResponseFlags(w, true, false)
		}, "/t/none", DegradedOther, DegradedOther},
		{"unknown reason collapses to other", func(w http.ResponseWriter) {
			MarkDegradedReason(w, "err: dial tcp 10.0.0.1: refused")
			MarkResponseFlags(w, true, false)
		}, "/t/unknown", DegradedOther, DegradedOther},
		{"age-only stale records no reason", func(w http.ResponseWriter) {
			MarkResponseFlags(w, false, true)
		}, "/t/stale", "", ""},
		{"reason without the degraded flag is not counted", func(w http.ResponseWriter) {
			MarkDegradedReason(w, DegradedRefreshFailed)
			MarkResponseFlags(w, false, true)
		}, "/t/noflag", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("GET "+tc.route, func(w http.ResponseWriter, _ *http.Request) { tc.mark(w) })
			h := HTTPMetrics(CaptureRoute(mux))
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, tc.route, nil))

			for _, reason := range []string{DegradedRefreshFailed, DegradedRefreshOverrun, DegradedProvisional, DegradedSectionDropped, DegradedOther} {
				want := 0.0
				if reason == tc.reason {
					want = 1
				}
				if got := testutil.ToFloat64(APIDegradedResponsesTotal.WithLabelValues(tc.route, reason)); got != want {
					t.Errorf("%s reason=%s = %v, want %v", tc.route, reason, got, want)
				}
			}
		})
	}
}

func TestDegradedReasonOf(t *testing.T) {
	var seen string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /t/of", func(w http.ResponseWriter, _ *http.Request) {
		MarkDegradedReason(w, DegradedSectionDropped)
		MarkResponseFlags(w, true, true)
	})
	h := HTTPMetrics(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r)
		seen = DegradedReasonOf(w)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/t/of", nil))
	if seen != DegradedSectionDropped {
		t.Errorf("DegradedReasonOf = %q, want %q", seen, DegradedSectionDropped)
	}
	if got := DegradedReasonOf(httptest.NewRecorder()); got != "" {
		t.Errorf("DegradedReasonOf on an unrecorded writer = %q, want empty", got)
	}
}
