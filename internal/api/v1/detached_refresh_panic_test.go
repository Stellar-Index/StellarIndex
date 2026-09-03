// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// panickingHist panics on its first N calls and then succeeds, so a test
// can assert both halves of the contract: the panic is contained, AND the
// cache entry it left behind is still RETRYABLE.
type panickingHist struct {
	HistoryReader
	calls     atomic.Int64
	panicUpTo int64
}

func (f *panickingHist) LatestTradePerSource(
	_ context.Context, _ canonical.Pair, _ string,
) ([]canonical.Trade, error) {
	if f.calls.Add(1) <= f.panicUpTo {
		// The shape the finding names: an index into an empty slice
		// returned by a degraded read.
		var degraded []canonical.Trade
		return []canonical.Trade{degraded[0]}, nil
	}
	return []canonical.Trade{{Source: "sdex"}}, nil
}

// TestCachedHistoryReader_ColdFillPanic_SurfacesErrorAndStaysRetryable pins
// what happens AFTER the panic is recovered, which is the part a bare
// `defer worker.Recover(...)` would have got wrong.
//
// The cold fill runs detached and its waiter wakes on close(done), reading
// entry.err / entry.trades across that edge. Recover-and-return leaves
// entry.at zero, entry.err nil and entry.flight set, so the waiter reads an
// EMPTY result with a NIL error — a 200 OK carrying no trades — and every
// later request re-joins the same already-closed flight and gets the same
// empty answer for the life of the process. Silent, permanent, and strictly
// worse than the crash it replaced.
//
// So this asserts the CORRECTED values, not merely that nothing crashed:
//
//  1. the waiter gets errHistoryFillPanicked, not (nil, nil);
//  2. the very next call re-runs the upstream — the entry was dropped, not
//     poisoned — and returns the real row;
//  3. stellarindex_worker_panics_total{worker="api-history-latest-per-source-fill"}
//     moved, which is what makes the dead refresher page an operator.
//
// Red without the fix: with only `defer close(done)` present the first call
// returns (nil, nil) and the second returns (nil, nil) without ever calling
// upstream again.
func TestCachedHistoryReader_ColdFillPanic_SurfacesErrorAndStaysRetryable(t *testing.T) {
	const workerName = "api-history-latest-per-source-fill"
	before := testutil.ToFloat64(obs.WorkerPanicsTotal.WithLabelValues(workerName))

	up := &panickingHist{panicUpTo: 1}
	c := NewCachedHistoryReader(up, time.Minute)
	p := histTestPair(t)

	rows, err := c.LatestTradePerSource(context.Background(), p, "")
	if !errors.Is(err, errHistoryFillPanicked) {
		t.Fatalf("cold waiter got (%v rows, err=%v), want errHistoryFillPanicked — a "+
			"panicking fill that reports success serves an EMPTY result as a 200 OK",
			len(rows), err)
	}

	// The entry must have been dropped, not left latched in flight.
	rows, err = c.LatestTradePerSource(context.Background(), p, "")
	if err != nil {
		t.Fatalf("second call err = %v, want nil — the panicked entry poisoned the "+
			"cache instead of being dropped for retry", err)
	}
	if len(rows) != 1 || rows[0].Source != "sdex" {
		t.Fatalf("second call rows = %#v, want exactly the upstream row — the cache "+
			"served the panicked fill's empty result instead of retrying", rows)
	}
	if got := up.calls.Load(); got != 2 {
		t.Fatalf("upstream called %d time(s), want 2 — the second request must start a "+
			"FRESH fill; anything less means the panicked flight was reused", got)
	}

	after := testutil.ToFloat64(obs.WorkerPanicsTotal.WithLabelValues(workerName))
	if after-before != 1 {
		t.Errorf("stellarindex_worker_panics_total{worker=%q} moved by %v, want 1 — "+
			"without it the refresher dies with no page and no runbook", workerName, after-before)
	}
}

// TestCachedHistoryReader_StaleRefreshPanic_KeepsStaleAndRetries is the
// (A') half: there IS a prior good value, so the correct degradation is to
// keep serving it and clear the in-flight marker so the next expiry
// retries. Recover-and-return would leave entry.flight set, and (A') only
// kicks a refresh when e.flight == nil — the key would serve that one stale
// value for the life of the process, never refreshing again.
func TestCachedHistoryReader_StaleRefreshPanic_KeepsStaleAndRetries(t *testing.T) {
	up := &panickingHist{}
	c := NewCachedHistoryReader(up, 20*time.Millisecond)
	p := histTestPair(t)

	// Warm it with a real value.
	if _, err := c.LatestTradePerSource(context.Background(), p, ""); err != nil {
		t.Fatalf("warm: %v", err)
	}
	// Arm the panic for the NEXT (stale-revalidate) call only.
	up.panicUpTo = 2
	time.Sleep(40 * time.Millisecond)

	// (A'): serves stale immediately and kicks the detached refresh.
	rows, err := c.LatestTradePerSource(context.Background(), p, "")
	if err != nil || len(rows) != 1 {
		t.Fatalf("stale serve = (%d rows, %v), want the retained stale value", len(rows), err)
	}
	if !waitFor(2*time.Second, func() bool { return up.calls.Load() >= 2 }) {
		t.Fatal("the detached stale refresh never ran")
	}

	// The marker must be clear, so a later expiry can refresh again.
	time.Sleep(40 * time.Millisecond)
	if _, err := c.LatestTradePerSource(context.Background(), p, ""); err != nil {
		t.Fatalf("post-panic serve: %v", err)
	}
	if !waitFor(2*time.Second, func() bool { return up.calls.Load() >= 3 }) {
		t.Fatal("no THIRD upstream call — the panicked refresh left entry.flight set, " +
			"so this key would serve its stale value for the life of the process")
	}
}
