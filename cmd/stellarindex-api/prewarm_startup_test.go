// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
)

// TestPrewarmCaches_FiresBothPassesBeforeTheFirstTick pins that both passes run immediately rather
// than waiting for their tickers. A heavy pass running first and sequentially (sources_stats alone
// takes about 8 s) would keep the user-facing listing keys cold for tens of seconds after restart.
func TestPrewarmCaches_FiresBothPassesBeforeTheFirstTick(t *testing.T) {
	t.Parallel()

	statsLog := newCallLog()
	stats := v1.NewCachedSourcesStatsReader(&stubSourcesStatsReader{log: statsLog}, 0)
	markets, marketsLog := newRecordingMarkets()
	assets := v1.NewCachedAssetsReader(&stubAssetsReader{}, 0)
	issuers := v1.NewCachedIssuersReader(&stubIssuersReader{}, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		// catalogueLen large enough that catalogueFillPrewarmOptions adds
		// nothing here — this test is about pass ordering, not catalogue fill.
		prewarmCaches(ctx, discardLogger(), stats, markets, assets, issuers, nil, nil, nil, noCatalogueFillTestLen)
	}()

	// Both passes must finish well inside the shorter (60s) cadence, which is what "immediately on startup" means. Readers
	// are in-memory, so the deadline is generous for the work yet far below the tick: a pass waiting for its first ticker
	// fails.
	deadline := time.After(20 * time.Second)
	for {
		if statsLog.has("GetSourceStats()") && len(marketsLog.signatures()) > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("prewarmCaches did not run both passes on startup within 20s:\n"+
				"  heavy (stats) calls : %v\n  light (markets) calls: %d\n\n"+
				"Both passes must fire before their tickers — a pass that waits for its "+
				"first tick leaves the API serving cold keys for up to its full cadence "+
				"after every deploy.", statsLog.signatures(), len(marketsLog.signatures()))
		case <-time.After(5 * time.Millisecond):
		}
	}

	// The heavy pass warms all three source-stats slots. 7d was missing though its cache slot existed, so the cold 7d read
	// hit the request path: 8.54s measured live against 1.09s for the same call without sparkline7d.
	for _, want := range []string{
		"GetSourceStats()",
		"GetSourceVolumeHistory24h()",
		"GetSourceVolumeHistory7d()",
	} {
		if !statsLog.has(want) {
			t.Errorf("prewarmHeavy never called %s; its cache slot exists, so leaving it "+
				"un-warmed just moves the cost onto the /dexes/{source} request path", want)
		}
	}

	// And it must return on cancellation rather than leak. run() starts
	// it detached, so a loop that ignores ctx.Done() is invisible until
	// shutdown hangs.
	cancel()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("prewarmCaches did not return within 20s of context cancellation")
	}
}
