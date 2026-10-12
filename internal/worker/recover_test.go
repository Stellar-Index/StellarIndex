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

// TestRecover_ContainsPanicAndLogs proves the guard (a) confines the panic so a detached worker cannot terminate the process, and
// (b) logs at Error with worker name, panic value and stack. Without recover() the panic would escape and crash the test binary.
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

// TestRecover_NilLoggerDoesNotItselfPanic guards the nil-logger branch: a recover helper that panicked on nil would turn a
// recoverable worker fault into an unrecoverable one. slog.Default() is the fallback sink.
func TestRecover_NilLoggerDoesNotItselfPanic(t *testing.T) {
	guardedWorker := func() {
		defer worker.Recover(nil, "nil-logger-worker")
		panic("simulated worker fault")
	}
	guardedWorker() // must return without panicking out
}

// TestRecover_CountsThePanic — the whole point: a recovered panic
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
