// Package worker holds shared primitives for the detached background worker goroutines the
// StellarIndex binaries spawn (refreshers, rollup loops, streamers, pollers, publishers).
package worker

import (
	"fmt"
	"log/slog"
	"runtime/debug"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// Recover logs a panic in a detached worker goroutine instead of crashing; defer it INSIDE each goroutine body, since recover() only catches the goroutine's own panic.
// The panicking worker stops and is not restarted, hence Error level plus the full stack.
func Recover(logger *slog.Logger, name string) {
	if r := recover(); r != nil {
		Report(logger, name, r)
	}
}

// Report records an ALREADY-RECOVERED panic: recover() works only one frame deep, so a caller with its own deferred function (stellarindex-api's recoverBackgroundWorker) hands the value here, and every binary moves the same counter the page rule reads.
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
