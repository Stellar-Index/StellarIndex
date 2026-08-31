package main

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// /v1/issuers had no prewarm at all while CachedIssuersReader's TTL is
// 5 minutes, so the slot expired every 5 minutes and the next caller
// paid a full cold fill. Measured on r1 2026-09-01: 1.212s cold vs
// 0.129s warm.
//
// At production's ~0.08 rps most requests arrive AFTER the TTL has
// lapsed, so the cold path was near the common case rather than a
// startup-only cost — it surfaced as recurring multi-second latency
// spikes that looked like they came and went at random.
//
// The query cannot be indexed out of the problem; CachedIssuersReader's
// own doc records why (two sequential scans feeding a HashAggregate
// over 57k groups, ~196ms at the database alone). Keeping the slot warm
// is the fix available.

// recordingIssuersReader records the limits it is asked for.
type recordingIssuersReader struct {
	mu     sync.Mutex
	limits []int
}

func (r *recordingIssuersReader) ListIssuers(
	_ context.Context, limit int,
) ([]timescale.IssuerSummary, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.limits = append(r.limits, limit)
	return []timescale.IssuerSummary{}, nil
}

func (r *recordingIssuersReader) GetIssuer(
	_ context.Context, _ string,
) (timescale.IssuerRow, error) {
	return timescale.IssuerRow{}, nil
}

func (r *recordingIssuersReader) ListIssuerAssets(
	_ context.Context, _ string,
) ([]timescale.IssuerAsset, error) {
	return []timescale.IssuerAsset{}, nil
}

func (r *recordingIssuersReader) seen() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := append([]int(nil), r.limits...)
	sort.Ints(out)
	return out
}

// TestPrewarmIssuersWarmsTheLimitsRealCallersUse is the guard. A warmed
// limit nobody requests is a phantom slot: it costs a query and leaves
// the actual caller on the cold path anyway. That exact mistake is
// recorded in prewarmLight for /v1/pools, where a mismatched cache key
// left every user request paying 10-30s against a cache that looked
// warm.
func TestPrewarmIssuersWarmsTheLimitsRealCallersUse(t *testing.T) {
	rec := &recordingIssuersReader{}
	cached := v1.NewCachedIssuersReader(rec, 5*time.Minute)

	prewarmIssuers(context.Background(), discardLogger(), cached)

	got := rec.seen()
	want := []int{1, 5, 100}
	if len(got) != len(want) {
		t.Fatalf("prewarmed limits = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("prewarmed limits = %v, want %v", got, want)
		}
	}
}

// TestPrewarmIssuersCoversTheHandlerDefault is the one that would have
// caught the original bug's most costly case. handleIssuersList falls
// back to limit=100 when the query parameter is absent, so a bare
// GET /v1/issuers lands on that key — and it is also what the sla-probe
// and the OpenAPI default send. If 100 ever drops out of the set, the
// most-requested slot goes cold every 5 minutes again.
func TestPrewarmIssuersCoversTheHandlerDefault(t *testing.T) {
	const handlerDefaultLimit = 100 // handleIssuersList: `limit := 100`
	for _, l := range prewarmIssuerLimits {
		if l == handlerDefaultLimit {
			return
		}
	}
	t.Fatalf("prewarmIssuerLimits %v omits the handler's default limit %d — a bare "+
		"GET /v1/issuers would cold-fill on every cache expiry (1.212s vs 0.129s on r1)",
		prewarmIssuerLimits, handlerDefaultLimit)
}

// TestPrewarmIssuersWarmsInsideTheCacheTTL — warming on a cadence LONGER
// than the TTL is indistinguishable from not warming at all: the slot
// expires between passes and the next caller still pays the cold fill.
// prewarmIssuers rides prewarmLight's 60s tick against a 5-minute TTL.
// This pins the relationship, not the individual numbers, so either can
// be tuned as long as the invariant holds.
func TestPrewarmIssuersWarmsInsideTheCacheTTL(t *testing.T) {
	const (
		lightCadence  = 60 * time.Second // prewarmCaches
		issuersCacheT = 5 * time.Minute  // NewCachedIssuersReader in main
		minHeadroom   = 2                // survive a dropped cycle
	)
	if lightCadence*minHeadroom >= issuersCacheT {
		t.Fatalf("prewarm cadence %v gives less than %dx headroom against the %v "+
			"issuers cache TTL — a single dropped cycle would expose a cold slot",
			lightCadence, minHeadroom, issuersCacheT)
	}
}

// TestPrewarmIssuersNilReaderIsSafe — a deployment with no issuers
// reader wired must not panic the prewarm goroutine, which runs
// detached and would take the whole cadence down with it.
func TestPrewarmIssuersNilReaderIsSafe(t *testing.T) {
	prewarmIssuers(context.Background(), discardLogger(), nil)
}
