package worker_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/obs"

	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// TestRecover_ContainsPanicAndLogs proves the guard does its two jobs:
//
//	(a) it CONFINES the panic — a deferred worker.Recover stops the panic
//	    from propagating past the function that deferred it, so a detached
//	    worker's fault no longer terminates the whole process; and
//	(b) it LOGS the fault at Error with the worker name, the panic value,
//	    and a stack, so a silently-stopped worker cannot pass unnoticed.
//
// Proven red: the pre-fix state of every worker this campaign guards was
// "no recover() anywhere in the goroutine's stack". If worker.Recover
// omitted its recover() (that pre-fix analog), the panic below escapes
// guardedWorker into this test function, which has no recover of its
// own, and crashes the test binary. With the guard, guardedWorker
// returns normally and the assertions run.
func TestRecover_ContainsPanicAndLogs(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError}))

	guardedWorker := func() {
		defer worker.Recover(logger, "unit-under-test")
		panic("simulated worker fault")
	}

	// If Recover fails to recover, this call panics out and the test
	// binary dies before reaching any assertion. Reaching the next line
	// at all is the (a) proof.
	guardedWorker()

	out := buf.String()
	for _, want := range []string{
		"background worker panicked", // the Error-level message
		"unit-under-test",            // the worker name we passed
		"simulated worker fault",     // the recovered panic value
		"\"stack\"",                  // a captured stack is present
	} {
		if !strings.Contains(out, want) {
			t.Errorf("recover log is missing %q\nfull log: %s", want, out)
		}
	}
}

// TestRecover_NilLoggerDoesNotItselfPanic guards the defensive nil-logger
// branch: a recover helper that panicked on a nil logger would be worse
// than no guard at all (it would turn a recoverable worker fault into an
// unrecoverable one). slog.Default() is used as the fallback sink.
func TestRecover_NilLoggerDoesNotItselfPanic(t *testing.T) {
	guardedWorker := func() {
		defer worker.Recover(nil, "nil-logger-worker")
		panic("simulated worker fault")
	}
	guardedWorker() // must return without panicking out
}

// TestRecover_CountsThePanic — the whole point of #368 M4: a recovered panic
// must leave a signal an alert can fire on, not just a log line. The
// counter is per worker so the alert names the dead goroutine.
func TestRecover_CountsThePanic(t *testing.T) {
	before := testutil.ToFloat64(obs.WorkerPanicsTotal.WithLabelValues("test-worker-m4"))
	func() {
		defer worker.Recover(nil, "test-worker-m4")
		panic("boom")
	}()
	after := testutil.ToFloat64(obs.WorkerPanicsTotal.WithLabelValues("test-worker-m4"))
	if after-before != 1 {
		t.Fatalf("stellarindex_worker_panics_total{worker=test-worker-m4} rose by %v, want exactly 1", after-before)
	}
	// A clean exit must NOT count.
	func() {
		defer worker.Recover(nil, "test-worker-m4")
	}()
	if got := testutil.ToFloat64(obs.WorkerPanicsTotal.WithLabelValues("test-worker-m4")); got != after {
		t.Fatalf("counter moved on a non-panicking exit (%v → %v)", after, got)
	}
}
