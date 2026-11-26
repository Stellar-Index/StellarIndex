package obs

import (
	"context"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
)

// Reasons a response carries flags.degraded. A closed set: they label [APIDegradedResponsesTotal], so a new one is a deliberate edit here.
const (
	DegradedRefreshFailed   = "refresh_failed"    // the last refresh of the served cache entry failed
	DegradedRefreshOverrun  = "refresh_overrun"   // the refresh in flight has outrun its budget
	DegradedRefreshRefused  = "refresh_refused"   // no refresh slot was free, so the last good copy was served unrefreshed
	DegradedSourceStale     = "source_stale"      // the underlying observation (supply, price) is itself old; a re-ask will not heal it
	DegradedProvisional     = "provisional"       // an older value was served while its refresh is in flight; a re-ask heals it
	DegradedSectionDropped  = "section_dropped"   // a panel or overlay was dropped from the body
	DegradedPartialRead     = "partial_read"      // an upstream read failed, so the body is a lower bound
	DegradedCheckUnread     = "check_unread"      // a gate (issuer directory, exclusion scope) could not be read, so the response withholds data
	DegradedRequestCanceled = "request_cancelled" // the request context died mid-build
	DegradedOther           = "other"             // set by a path that does not name its cause
)

// degradedPrecedence ranks the reasons, most data-affecting first. When several causes hold on one response the
// earliest here is the one counted; this is the only place the order lives.
var degradedPrecedence = []string{
	DegradedCheckUnread, DegradedSectionDropped, DegradedPartialRead, DegradedRefreshFailed,
	DegradedRefreshOverrun, DegradedRefreshRefused, DegradedSourceStale, DegradedProvisional, DegradedRequestCanceled,
}

// degradedRank is the position in [degradedPrecedence]; "other" and anything unknown rank last.
func degradedRank(reason string) int {
	for i, r := range degradedPrecedence {
		if r == reason {
			return i
		}
	}
	return len(degradedPrecedence)
}

// APIDegradedResponsesTotal counts API responses written with flags.degraded by route pattern and cause; synthetic probes are excluded as in [HTTPRequestsTotal].
var APIDegradedResponsesTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_api_degraded_responses_total",
		Help: "API responses served with flags.degraded, labelled by route pattern and reason (check_unread|section_dropped|partial_read|refresh_failed|refresh_overrun|refresh_refused|source_stale|provisional|request_cancelled|other).",
	},
	[]string{"route", "reason"},
)

func normalizeDegradedReason(reason string) string {
	if degradedRank(reason) < len(degradedPrecedence) {
		return reason
	}
	return DegradedOther
}

// HighestDegradedReason returns the highest-precedence of reasons, "" for none; for a caller that must store the reason
// (a cached response) rather than note it on the request.
func HighestDegradedReason(reasons ...string) string {
	best := ""
	for _, r := range reasons {
		if best == "" || degradedRank(r) < degradedRank(best) {
			best = r
		}
	}
	return best
}

// markReason keeps the highest-precedence reason seen; "other" yields to any named one.
func (r *statusRecorder) markReason(reason string) {
	reason = normalizeDegradedReason(reason)
	r.reasonMu.Lock()
	defer r.reasonMu.Unlock()
	if r.reason == "" || degradedRank(reason) < degradedRank(r.reason) {
		r.reason = reason
	}
}

// MarkDegradedReason records why a response is degraded, on the recorder [MarkResponseFlags] reaches; the
// highest-precedence reason wins (see [degradedPrecedence]) and an unknown one becomes "other".
func MarkDegradedReason(w http.ResponseWriter, reason string) {
	if rec := findRecorder(w); rec != nil {
		rec.markReason(reason)
	}
}

type recorderKey struct{}

// NoteDegradedCause is MarkDegradedReason for code that holds only the request context (a cache serving a last-good
// copy). It records nothing unless the response ends up flagged degraded, and outside [HTTPMetrics] it is a no-op.
func NoteDegradedCause(ctx context.Context, reason string) {
	if rec, ok := ctx.Value(recorderKey{}).(*statusRecorder); ok {
		rec.markReason(reason)
	}
}

// DegradedReasonOf returns the reason a finished response was degraded ("other" when its path named none), or "" when it was not degraded.
func DegradedReasonOf(w http.ResponseWriter) string {
	rec := findRecorder(w)
	if rec == nil || !rec.degraded {
		return ""
	}
	return rec.reasonOrOther()
}

func (r *statusRecorder) reasonOrOther() string {
	r.reasonMu.Lock()
	defer r.reasonMu.Unlock()
	if r.reason == "" {
		return DegradedOther
	}
	return r.reason
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
