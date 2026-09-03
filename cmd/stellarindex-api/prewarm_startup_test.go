// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
)

// TestPrewarmCaches_FiresBothPassesBeforeTheFirstTick covers the outer
// loop, whose one startup-critical behaviour is that BOTH passes run
// immediately rather than waiting for their tickers (5 min / 60 s).
//
// The heavy pass used to run FIRST and SEQUENTIALLY. Its sources_stats
// query alone takes ~8s, so for tens of seconds after every restart the
// user-facing listing keys were still cold behind it. Measured on r1
// 2026-09-01, API up at 05:10:16:
//
//	05:10:37  10025 ms  /v1/assets?include=sparkline&limit=10&order_by=…
//	05:10:37  10026 ms  /v1/assets?limit=50
//
// Real users, 21s after boot, paying a cold fill for a key the prewarm
// had not reached yet. Steady state was already healthy, so this is
// purely a startup-window defect — and it recurs on every deploy, which
// is what makes it worth a pinned property rather than a one-off fix.
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
		prewarmCaches(ctx, discardLogger(), stats, markets, assets, issuers, nil, nil)
	}()

	// Both passes must complete well inside the shorter (60s) cadence,
	// which is what "immediately on startup" means. Every reader here is
	// in-memory, so the deadline is generous relative to the work and far
	// below the tick: a pass that waited for its first ticker fails here.
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

	// The heavy pass warms all three source-stats slots. 7d was missing
	// from this loop while its cache slot already existed, so the
	// un-warmed 7d read was paid on the request path instead: measured
	// 8.54s live, against 1.09s for the same call without sparkline7d.
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
