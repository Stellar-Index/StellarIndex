// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
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

// /v1/network/stats (CachedNetworkStatsReader) was never threaded into prewarmCaches/prewarmLight, so its slot stayed
// cold. The first request after each restart paid the full ~485ms p95 aggregate inline.

// recordingNetworkStatsReader records how many times it was called.
type recordingNetworkStatsReader struct {
	mu    sync.Mutex
	calls int
}

func (r *recordingNetworkStatsReader) GetNetworkStats(_ context.Context) (timescale.NetworkStats, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return timescale.NetworkStats{}, nil
}

func (r *recordingNetworkStatsReader) seen() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// Guard: prewarmLight, which runs on both the startup pass and the ticker, must warm the /v1/network/stats slot.
// CachedNetworkStatsReader must be passed to it, or this reader is never called.
func TestPrewarmLightWarmsNetworkStats(t *testing.T) {
	rec := &recordingNetworkStatsReader{}
	cached := v1.NewCachedNetworkStatsReader(rec, 30*time.Second)

	markets, _ := newRecordingMarkets()
	assets := v1.NewCachedAssetsReader(&stubAssetsReader{}, 0)
	issuers := v1.NewCachedIssuersReader(&stubIssuersReader{}, 0)

	// catalogueLen large enough that catalogueFillPrewarmOptions adds
	// nothing here — this test is about the network-stats slot, not catalogue fill.
	prewarmLight(context.Background(), discardLogger(), markets, assets, issuers, nil, nil, cached, noCatalogueFillTestLen)

	if got := rec.seen(); got != 1 {
		t.Fatalf("network stats prewarm calls = %d, want 1 — the /v1/network/stats "+
			"SWR slot is never warmed and the first post-restart request pays the "+
			"full upstream aggregate inline", got)
	}
}

// TestPrewarmNetworkStatsNilReaderIsSafe — a deployment with no network
// stats reader wired must not panic the detached prewarm goroutine.
func TestPrewarmNetworkStatsNilReaderIsSafe(t *testing.T) {
	prewarmNetworkStats(context.Background(), discardLogger(), nil)
}

// Drives the real auth middleware with the exact self-prewarm request (no credential, own User-Agent) and requires the
// start gate to agree: never start where every warm-up 401s, always start where the request is served.
func TestSelfPrewarmAdmitted_MatchesAuthMiddleware(t *testing.T) {
	t.Parallel()
	cases := []struct {
		mode         string
		wantAdmitted bool
	}{
		{"", true},
		{"none", true},
		{"apikey_optional", true},
		{"apikey", false},
		{"sep10", false},
		{"api-key", false}, // unknown mode: refuse rather than guess
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			t.Parallel()
			if got := selfPrewarmAdmitted(tc.mode); got != tc.wantAdmitted {
				t.Fatalf("selfPrewarmAdmitted(%q) = %v, want %v", tc.mode, got, tc.wantAdmitted)
			}
			var h http.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			})
			if mw := buildAuthMiddleware(tc.mode, authValidatorOptions{}, discardLogger()); mw != nil {
				h = mw(h)
			}
			req := httptest.NewRequest(http.MethodGet, "/v1/assets/native", nil)
			req.Header.Set("User-Agent", "stellarindex-prewarm/1")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if selfPrewarmAdmitted(tc.mode) && w.Code != http.StatusOK {
				t.Errorf("mode %q: self-prewarm would start but its request gets %d, "+
					"so every 60s pass is a no-op", tc.mode, w.Code)
			}
			if !tc.wantAdmitted && tc.mode != "api-key" && w.Code != http.StatusUnauthorized {
				t.Errorf("mode %q: credential-less request got %d, want 401 — the "+
					"premise for disabling the self-prewarm no longer holds", tc.mode, w.Code)
			}
		})
	}
}
