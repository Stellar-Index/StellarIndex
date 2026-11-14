package v1

import (
	"context"
	"sync/atomic"
)

// readReport collects, for one response, reads that failed (unread) or answered from an expired cache entry (stale). It rides
// the context because the cached readers satisfy interfaces whose signatures carry neither.
type readReport struct{ unread, stale atomic.Bool }

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
