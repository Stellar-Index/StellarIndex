package obs

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
)

// Reasons a response carries flags.degraded. A closed set: they label [APIDegradedResponsesTotal], so a new one is a deliberate edit here.
const (
	DegradedRefreshFailed  = "refresh_failed"  // the last refresh of the served cache entry failed or was refused
	DegradedRefreshOverrun = "refresh_overrun" // the refresh in flight has outrun its budget
	DegradedProvisional    = "provisional"     // the cached payload declared itself provisional
	DegradedSectionDropped = "section_dropped" // a cold cache dropped a panel from the body
	DegradedOther          = "other"           // set by a path that does not name its cause
)

// APIDegradedResponsesTotal counts API responses written with flags.degraded by route pattern and cause; synthetic probes are excluded as in [HTTPRequestsTotal].
var APIDegradedResponsesTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_api_degraded_responses_total",
		Help: "API responses served with flags.degraded, labelled by route pattern and reason (refresh_failed|refresh_overrun|provisional|section_dropped|other).",
	},
	[]string{"route", "reason"},
)

func normalizeDegradedReason(reason string) string {
	switch reason {
	case DegradedRefreshFailed, DegradedRefreshOverrun, DegradedProvisional, DegradedSectionDropped:
		return reason
	}
	return DegradedOther
}

// MarkDegradedReason records why a response is degraded, on the recorder [MarkResponseFlags] reaches; the first reason wins and an unknown one becomes "other".
func MarkDegradedReason(w http.ResponseWriter, reason string) {
	if rec := findRecorder(w); rec != nil && rec.reason == "" {
		rec.reason = normalizeDegradedReason(reason)
	}
}

// DegradedReasonOf returns the reason a finished response was degraded ("other" when its path named none), or "" when it was not degraded.
func DegradedReasonOf(w http.ResponseWriter) string {
	rec := findRecorder(w)
	if rec == nil || !rec.degraded {
		return ""
	}
	if rec.reason == "" {
		return DegradedOther
	}
	return rec.reason
}

func findRecorder(w http.ResponseWriter) *statusRecorder {
	for w != nil {
		if rec, ok := w.(*statusRecorder); ok {
			return rec
		}
		u, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return nil
		}
		w = u.Unwrap()
	}
	return nil
}
