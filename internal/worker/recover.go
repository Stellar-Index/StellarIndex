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

// Recover turns a panic in a DETACHED background worker goroutine into a
// logged error instead of a whole-process crash.
//
// An unrecovered panic in ANY goroutine terminates the entire Go process — it
// is not confined to the goroutine that panicked. So every long-running worker
// spawned with `go` must register this guard, or a single panic in one worker
// takes the whole binary down along with every healthy unit of work in flight.
//
// It MUST be invoked as a deferred call from INSIDE the goroutine body:
//
//	go func() {
//		defer worker.Recover(logger, "my-worker")
//		runForever(ctx)
//	}()
//
// recover() only fires when called from a function the panicking goroutine
// itself deferred, so this cannot be hoisted into a helper the goroutine
// merely calls — it has to be deferred within the goroutine's own stack. This
// is why a caller-side guard (e.g. the API binary's recoverBackgroundWorker
// wrapping the goroutine that CALLS a Run method) does not protect the inner
// per-item goroutines that Run itself fans out; each of those needs its own
// deferred Recover.
//
// The trade-off is stated rather than hidden: the panicking worker STOPS (its
// goroutine unwinds and is not restarted), so a crash-looping worker becomes a
// silently idle one instead of a crash-looping process. That is the better
// failure for a background worker whose halt should not take down its healthy
// siblings, but it is a real degradation — hence Error level plus the full
// stack, so it cannot pass unnoticed. Mirrors the stellarindex-api binary's
// local recoverBackgroundWorker exactly (log-only; no restart).
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
	// Count before logging so the metric exists even if logging fails
	// (#368 M4): this is the ONLY signal that a worker is now dead.
	obs.WorkerPanicsTotal.WithLabelValues(name).Inc()
	logger.Error("background worker panicked — worker STOPPED, process still running",
		"worker", name,
		"panic", fmt.Sprintf("%v", r),
		"stack", string(debug.Stack()))
}
