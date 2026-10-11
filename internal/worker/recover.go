// Package worker holds shared primitives for the detached background
// worker goroutines the StellarIndex binaries spawn — VWAP / supply
// refreshers, rollup loops, external-source streamers and pollers, the
// SSE price publisher, and the like.
package worker

import (
	"fmt"
	"log/slog"
	"runtime/debug"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// Recover turns a panic in a DETACHED background worker goroutine into a logged
// error instead of a whole-process crash (an unrecovered panic in ANY goroutine
// kills the process). Every long-running `go` worker must register it.
//
// It MUST be deferred from INSIDE the goroutine body:
//
//	go func() {
//		defer worker.Recover(logger, "my-worker")
//		runForever(ctx)
//	}()
//
// recover() only fires from a function the panicking goroutine itself deferred,
// so this cannot be hoisted into a helper the goroutine merely calls. A guard
// around the goroutine that CALLS a Run method does not protect the inner
// per-item goroutines Run fans out; each needs its own deferred Recover.
//
// The panicking worker STOPS (not restarted): a crash-looping worker becomes a
// silently idle one, better than taking down healthy siblings but a real
// degradation, hence Error level plus the full stack.
func Recover(logger *slog.Logger, name string) {
	if r := recover(); r != nil {
		Report(logger, name, r)
	}
}

// Report records an ALREADY-RECOVERED panic. It exists because recover()
// only works one frame deep: a caller with its own deferred function
// (stellarindex-api's recoverBackgroundWorker, which carries a different
// log message about the API still serving) cannot delegate to [Recover]
// and must hand the recovered value here instead. Every binary therefore
// moves the same counter, which is what the page rule reads.
func Report(logger *slog.Logger, name string, r any) {
	if logger == nil {
		logger = slog.Default()
	}
	// Count before logging so the metric exists even if logging fails:
	// this is the ONLY signal that a worker is now dead.
	obs.WorkerPanicsTotal.WithLabelValues(name).Inc()
	logger.Error("background worker panicked — worker STOPPED, process still running",
		"worker", name,
		"panic", fmt.Sprintf("%v", r),
		"stack", string(debug.Stack()))
}
