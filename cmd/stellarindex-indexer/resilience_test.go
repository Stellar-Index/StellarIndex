package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// TestRetryUntil_RetriesUntilTheDependencyComesBack is the behavioural
// half of #368 M11. startSignerTagger used to dial ClickHouse ONCE, on
// the caller's goroutine, and a single failure disabled AMM signer
// attribution for the lifetime of the process — so a ClickHouse restart
// during a deploy left trades.signer NULL until somebody restarted the
// indexer. The fix is not "log it louder", it is "keep trying", and the
// property that matters is that attempt N+1 happens at all.
func TestRetryUntil_RetriesUntilTheDependencyComesBack(t *testing.T) {
	calls := 0
	start := time.Now()
	ok := retryUntil(context.Background(), nil, "test dependency",
		2*time.Millisecond, 20*time.Millisecond,
		func(context.Context) error {
			calls++
			if calls < 3 {
				return errors.New("connection refused")
			}
			return nil
		})
	if !ok {
		t.Fatal("retryUntil gave up on a dependency that came back — the pre-fix behaviour")
	}
	if calls != 3 {
		t.Errorf("attempt called %d time(s), want exactly 3 (two failures then success)", calls)
	}
	// 2ms + 4ms of backoff must actually have been waited out; a retry
	// loop that busy-spins would hammer a struggling dependency.
	if elapsed := time.Since(start); elapsed < 6*time.Millisecond {
		t.Errorf("retries took %s, want at least 6ms of backoff (2ms + 4ms)", elapsed)
	}
}

// TestRetryUntil_SucceedsFirstTimeWithoutWaiting pins the common case:
// no sleep, no log, exactly one call.
func TestRetryUntil_SucceedsFirstTimeWithoutWaiting(t *testing.T) {
	calls := 0
	start := time.Now()
	ok := retryUntil(context.Background(), nil, "test dependency",
		time.Hour, time.Hour, // would be obvious if it ever slept
		func(context.Context) error { calls++; return nil })
	if !ok || calls != 1 {
		t.Fatalf("ok=%v calls=%d, want true/1", ok, calls)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("a first-attempt success waited %s", elapsed)
	}
}

// TestRetryUntil_StopsOnCancellation is the other half of the contract:
// the retry must not outlive the shutdown, and it must report failure so
// the caller abandons the work rather than proceeding with a nil handle.
func TestRetryUntil_StopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	done := make(chan bool, 1)
	go func() {
		done <- retryUntil(ctx, nil, "test dependency",
			10*time.Millisecond, time.Second,
			func(context.Context) error {
				calls++
				if calls == 1 {
					// Cancel from inside the first attempt so the loop
					// is provably in its backoff wait when it happens.
					cancel()
				}
				return errors.New("still down")
			})
	}()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("retryUntil reported success after cancellation — the caller would then use a handle it never got")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("retryUntil did not return after ctx cancellation; shutdown would hang on it")
	}
	if calls != 1 {
		t.Errorf("attempt called %d time(s) after cancellation, want 1", calls)
	}
}

// TestWaitBounded_ReturnsFalseWhenTheWaitOutlivesTheBudget covers the
// #368 LOW on the indexer's shutdown path. externalWait() used to be
// called bare, between two deadline-bounded steps, so one wedged CEX/FX
// connector held the whole binary until systemd escalated to SIGKILL —
// the one shutdown that skips the sink drain entirely.
//
// The false return is load-bearing, not cosmetic: main uses it to decide
// NOT to close the shared events channel, because a connector that is
// still running is still a sender and a send on a closed channel panics.
func TestWaitBounded_ReturnsFalseWhenTheWaitOutlivesTheBudget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	release := make(chan struct{})
	defer close(release)

	start := time.Now()
	if waitBounded(ctx, nil, "wedged-connector", func() { <-release }) {
		t.Fatal("waitBounded reported a drain that never happened; main would then close the events channel under a live sender")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("waitBounded took %s to give up on a 20ms budget", elapsed)
	}
}

// TestWaitBounded_ReturnsTrueOnCleanDrain is the happy path — the answer
// main needs before it may close the events channel.
func TestWaitBounded_ReturnsTrueOnCleanDrain(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	drained := false
	if !waitBounded(ctx, nil, "clean-connector", func() { drained = true }) {
		t.Fatal("waitBounded reported failure for a wait that completed")
	}
	if !drained {
		t.Error("the wait function was never called")
	}
}

// TestWaitBounded_PanicIsCountedAndReportedAsNotDrained: a fault inside a
// connector's teardown leaves us unable to say whether its senders have
// stopped, so the honest answer is "not drained" — the direction that
// skips close(events). And it must not crash the process on the way out:
// a panic in this goroutine would skip main's defers and take the sink
// drain with it, which is exactly what the shutdown path exists to run.
func TestWaitBounded_PanicIsCountedAndReportedAsNotDrained(t *testing.T) {
	const workerName = "panicking-connector-drain"
	before := testutil.ToFloat64(obs.WorkerPanicsTotal.WithLabelValues(workerName))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if waitBounded(ctx, nil, workerName, func() { panic("teardown fault") }) {
		t.Fatal("a faulted drain was reported as clean")
	}

	after := testutil.ToFloat64(obs.WorkerPanicsTotal.WithLabelValues(workerName))
	if after-before != 1 {
		t.Errorf("stellarindex_worker_panics_total{worker=%q} rose by %v, want exactly 1 — "+
			"an uncounted worker panic is an invisible death", workerName, after-before)
	}
}
