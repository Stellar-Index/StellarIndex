// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package explorer

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// panickingHoldersReader panics on its first N AssetHolders calls and then
// succeeds, so a test can assert both halves of the contract: the panic is
// contained, AND the flight it left behind is still retryable.
type panickingHoldersReader struct {
	*capReader
	calls     atomic.Int32
	panicUpTo int32
}

func (r *panickingHoldersReader) AssetHolders(
	context.Context, string, int,
) ([]clickhouse.AssetHolder, int64, error) {
	if r.calls.Add(1) <= r.panicUpTo {
		// The shape the finding names: an index into an empty slice
		// returned by a degraded read.
		var degraded []clickhouse.AssetHolder
		return []clickhouse.AssetHolder{degraded[0]}, 0, nil
	}
	return []clickhouse.AssetHolder{{AccountID: validTestAccount, Balance: 7}}, 1, nil
}

// TestAssetHoldersRefreshPanic_EndsFlightAndStaysRetryable pins what
// happens AFTER a panicking detached refresh is recovered — the part a bare
// `defer worker.Recover(...)` would have got wrong.
//
// perKeyFlight.end is what BOTH deregisters the flight and closes its done
// channel, and it was the last statement on every path rather than a defer.
// Recover-and-return therefore leaves the flight in perKeyFlight.inGoing
// forever with a channel that never closes: the cold waiter blocks to its
// own deadline, and every later request calls begin(key), is handed that
// same dead flight as a NON-owner, and blocks too. The asset's holders
// board can never be computed again for the life of the process.
//
// So this asserts the CORRECTED values, not merely that nothing crashed:
//
//  1. the cold waiter is released promptly with errRefreshPanicked rather
//     than blocking until its context expires;
//  2. the flight was deregistered, so the NEXT request owns a fresh one and
//     gets the real board;
//  3. stellarindex_worker_panics_total{worker="explorer-asset-holders-refresh"}
//     moved, which is what pages an operator about the dead refresher.
//
// Red without the fix: the first call blocks for the full request deadline
// and returns ctx.Err(), and the second does the same, never re-reading.
func TestAssetHoldersRefreshPanic_EndsFlightAndStaysRetryable(t *testing.T) {
	const workerName = "explorer-asset-holders-refresh"
	before := testutil.ToFloat64(obs.WorkerPanicsTotal.WithLabelValues(workerName))

	h, _ := newSWRHandler()
	reader := &panickingHoldersReader{capReader: &capReader{probe: &deadlineProbe{}}, panicUpTo: 1}
	h.Reader = reader

	// A generous request deadline: if the flight were left unended this
	// would block for the whole of it, which is the failure being pinned.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	_, _, _, _, err := h.assetHoldersCached(ctx, "USDC-"+validTestAccount, 10)
	if !errors.Is(err, errRefreshPanicked) {
		t.Fatalf("cold waiter err = %v, want errRefreshPanicked — a flight that is "+
			"never ended leaves every waiter blocked until its own deadline", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("cold waiter took %v — it waited out its own deadline instead of "+
			"being released by the panicking flight's end()", elapsed)
	}

	// The flight must have been deregistered, so this request OWNS a new one.
	holders, total, _, _, err := h.assetHoldersCached(ctx, "USDC-"+validTestAccount, 10)
	if err != nil {
		t.Fatalf("second call err = %v, want nil — the panicked flight was left "+
			"registered, so this request joined a dead one", err)
	}
	if len(holders) != 1 || holders[0].Balance != 7 || total != 1 {
		t.Fatalf("second call = (%#v, total=%d), want the real board — the panicked "+
			"refresh was reused instead of retried", holders, total)
	}
	if got := reader.calls.Load(); got != 2 {
		t.Fatalf("reader called %d time(s), want 2 — anything less means the second "+
			"request did not start a fresh refresh", got)
	}

	after := testutil.ToFloat64(obs.WorkerPanicsTotal.WithLabelValues(workerName))
	if after-before != 1 {
		t.Errorf("stellarindex_worker_panics_total{worker=%q} moved by %v, want 1 — "+
			"without it the refresher dies with no page and no runbook", workerName, after-before)
	}
}
