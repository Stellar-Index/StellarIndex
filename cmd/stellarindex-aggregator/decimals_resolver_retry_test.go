package main

// The decimals-assumption guard must be DELAYED by a cold lake, never
// disabled by one (audit-2026-09-02 F040).
//
// The resolver used to be dialled inline at startup: one failed ping
// emitted a single WARN and the guard — Backfill and periodic Sweep both
// — never existed for that process. That is the expected outcome of a
// reboot, not an edge case: clickhouse-server spends minutes loading
// metadata for the 150B-row lake and the aggregator unit's ordering does
// not wait for it. A non-7-decimal SEP-41 token listing afterwards then
// gets no nonstandard_decimals_assets row, aggregate.AdjustPrice applies
// no correction, and every served price on its pairs is skewed by
// 10^(7-decimals) — silently, since no metric separates "the guard found
// nothing" from "the guard never ran".
//
// Proven red against the pre-fix wiring (a single
// clickhouse.NewExplorerReader call whose error logged and fell
// through): one dial, no retry, guard never armed.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// shortenDecimalsResolverBackoff makes the retry loop test-fast without
// changing its shape.
func shortenDecimalsResolverBackoff(t *testing.T) {
	t.Helper()
	oldMin, oldMax := decimalsResolverRetryMin, decimalsResolverRetryMax
	decimalsResolverRetryMin = time.Millisecond
	decimalsResolverRetryMax = 4 * time.Millisecond
	t.Cleanup(func() {
		decimalsResolverRetryMin, decimalsResolverRetryMax = oldMin, oldMax
	})
}

func TestDialDecimalsResolver_ColdLakeDelaysTheGuardRatherThanDisablingIt(t *testing.T) {
	shortenDecimalsResolverBackoff(t)

	// A ClickHouse still loading metadata: the first dials fail, then it
	// answers. The shipped reader value is opaque here — what matters is
	// that the guard gets one at all.
	const failures = 5
	var attempts atomic.Int32
	dial := func(context.Context, string) (*clickhouse.ExplorerReader, error) {
		if attempts.Add(1) <= failures {
			return nil, errors.New("clickhouse: ping explorer reader 127.0.0.1:9300: dial tcp: connection refused")
		}
		return &clickhouse.ExplorerReader{}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	reader, ok := dialDecimalsResolver(ctx, quietLogger(), "127.0.0.1:9300", dial)
	if !ok {
		t.Fatalf("resolver gave up after %d failed dials — a cold lake must delay the "+
			"decimals guard, not disable it for the process lifetime", attempts.Load())
	}
	if reader == nil {
		t.Fatal("ok=true with a nil reader — the guard would be built on nothing")
	}
	if got := attempts.Load(); got != failures+1 {
		t.Errorf("dial attempts = %d, want %d (%d failures then the success) — "+
			"the pre-fix wiring dialled exactly once and gave up", got, failures+1, failures)
	}
}

// Shutdown must still stop the loop promptly: a retry that ignores
// cancellation would hold the refresher WaitGroup open and hang the
// aggregator's shutdown.
func TestDialDecimalsResolver_StopsOnShutdown(t *testing.T) {
	shortenDecimalsResolverBackoff(t)

	var attempts atomic.Int32
	dial := func(context.Context, string) (*clickhouse.ExplorerReader, error) {
		attempts.Add(1)
		return nil, errors.New("clickhouse: unreachable")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		reader, ok := dialDecimalsResolver(ctx, quietLogger(), "127.0.0.1:9300", dial)
		if ok {
			t.Errorf("ok=true from a resolver that never answered")
		}
		if reader != nil {
			t.Errorf("reader = %v on the shutdown path, want nil", reader)
		}
	}()

	// Let it fail a few times, then shut down.
	deadline := time.After(2 * time.Second)
	for attempts.Load() < 2 {
		select {
		case <-deadline:
			t.Fatal("the resolver did not retry at all before shutdown")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("dialDecimalsResolver did not return after context cancellation — " +
			"shutdown would hang on the refresher WaitGroup")
	}
}

// A dial that succeeds first time must not pay any backoff: the common
// case is a warm lake and the guard has to arm immediately.
func TestDialDecimalsResolver_WarmLakeArmsImmediately(t *testing.T) {
	var attempts atomic.Int32
	dial := func(context.Context, string) (*clickhouse.ExplorerReader, error) {
		attempts.Add(1)
		return &clickhouse.ExplorerReader{}, nil
	}

	start := time.Now()
	reader, ok := dialDecimalsResolver(context.Background(), quietLogger(), "127.0.0.1:9300", dial)
	elapsed := time.Since(start)

	if !ok || reader == nil {
		t.Fatalf("warm dial returned ok=%v reader=%v, want a reader", ok, reader)
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("dial attempts = %d on a healthy lake, want 1", got)
	}
	if elapsed >= decimalsResolverRetryMin {
		t.Errorf("a successful first dial waited %v; it must not pay the backoff", elapsed)
	}
}
