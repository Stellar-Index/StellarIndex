package main

import (
	"context"
	"sync"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// /v1/network/stats (CachedNetworkStatsReader) was constructed in main but
// never threaded into prewarmCaches/prewarmLight, so the slot was never
// warmed (RLT-287) — the first request after every binary restart paid
// the full ~485ms p95 network-wide aggregate inline instead of getting a
// warm value.

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

// TestPrewarmLightWarmsNetworkStats is the guard: prewarmLight — the
// function the prewarm goroutine actually runs on both its startup pass
// and its steady-state ticker — must warm the /v1/network/stats slot.
// Before the fix, CachedNetworkStatsReader was never passed to
// prewarmLight at all, so this reader was never called.
func TestPrewarmLightWarmsNetworkStats(t *testing.T) {
	rec := &recordingNetworkStatsReader{}
	cached := v1.NewCachedNetworkStatsReader(rec, 30*time.Second)

	markets, _ := newRecordingMarkets()
	assets := v1.NewCachedAssetsReader(&stubAssetsReader{}, 0)
	issuers := v1.NewCachedIssuersReader(&stubIssuersReader{}, 0)

	// catalogueLen large enough that catalogueFillPrewarmOptions adds
	// nothing here — this test is about the network-stats slot, not T279.
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
