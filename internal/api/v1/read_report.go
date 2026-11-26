package v1

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// readReport collects, for one response, reads that failed (unread), answered from an expired cache entry (stale) or were
// withheld by a source exclusion (withheld). It rides the context because the cached readers satisfy interfaces whose
// signatures carry none of them.
type readReport struct {
	unread, stale, withheld atomic.Bool
	mu                      sync.Mutex
	// degraded is the highest reason a read made the response degraded (a last-good copy, a failed leg); stale alone never sets it.
	degraded string
}

// reportDegradedRead records reason ("" is a no-op) on the request and on ctx's report, if any.
func reportDegradedRead(ctx context.Context, reason string) {
	if reason == "" {
		return
	}
	obs.NoteDegradedCause(ctx, reason)
	if rep, ok := ctx.Value(readReportKey{}).(*readReport); ok {
		rep.mu.Lock()
		rep.degraded = obs.HighestDegradedReason(rep.degraded, reason)
		rep.mu.Unlock()
	}
}

// degradedReason is the reason a read reported through [reportDegradedRead], "" for none.
func (r *readReport) degradedReason() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.degraded
}

type readReportKey struct{}

func withReadReport(ctx context.Context) (context.Context, *readReport) {
	rep := &readReport{}
	return context.WithValue(ctx, readReportKey{}, rep), rep
}

// reportStaleRead and reportUnreadRead are no-ops on a context with no report.
func reportStaleRead(ctx context.Context) {
	if rep, ok := ctx.Value(readReportKey{}).(*readReport); ok {
		rep.stale.Store(true)
	}
}

func reportUnreadRead(ctx context.Context) {
	if rep, ok := ctx.Value(readReportKey{}).(*readReport); ok {
		rep.unread.Store(true)
	}
}

// reportWithheldRead marks a value an in-force exclusion withheld, which is otherwise the same body as a value that does not
// exist; the response is degraded so no shared cache holds it past a lift.
func reportWithheldRead(ctx context.Context) {
	if rep, ok := ctx.Value(readReportKey{}).(*readReport); ok {
		rep.withheld.Store(true)
	}
}
