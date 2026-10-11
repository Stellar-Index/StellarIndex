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

// /v1/issuers is prewarmed because CachedIssuersReader's 5 minute TTL lapses before most requests
// arrive at low traffic, and the cold fill is about 1.2 s against 0.13 s warm. The query cannot be
// indexed away (see CachedIssuersReader's doc), so keeping the slot warm is the fix.

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

// TestPrewarmIssuersWarmsTheLimitsRealCallersUse pins that the warmed limits are ones callers
// use. A slot no caller lands on costs a query and leaves the real caller cold.
func TestPrewarmIssuersWarmsTheLimitsRealCallersUse(t *testing.T) {
	rec := &recordingIssuersReader{}
	cached := v1.NewCachedIssuersReader(rec, 5*time.Minute)

	prewarmIssuers(context.Background(), discardLogger(), cached)
	warmed := rec.seen()
	if len(warmed) == 0 {
		t.Fatal("prewarm made no upstream call")
	}

	for _, lim := range prewarmIssuerLimits {
		if _, err := cached.ListIssuers(context.Background(), lim); err != nil {
			t.Fatal(err)
		}
	}
	if got := rec.seen(); len(got) != len(warmed) {
		t.Fatalf("real-caller limits %v reached upstream after prewarm: calls %v -> %v",
			prewarmIssuerLimits, warmed, got)
	}
}

// TestPrewarmIssuersCoversTheHandlerDefault pins the handler's default limit (100, used by a bare
// GET /v1/issuers, the sla-probe and the OpenAPI default) staying in the set, so the
// most-requested slot does not go cold every 5 minutes.
func TestPrewarmIssuersCoversTheHandlerDefault(t *testing.T) {
	for _, l := range prewarmIssuerLimits {
		if l == v1.IssuersListDefaultLimit {
			return
		}
	}
	t.Fatalf("prewarmIssuerLimits %v omits the handler's default limit %d — a bare "+
		"GET /v1/issuers would cold-fill on every cache expiry (1.212s vs 0.129s on r1)",
		prewarmIssuerLimits, v1.IssuersListDefaultLimit)
}

// TestPrewarmIssuersWarmsInsideTheCacheTTL pins that the warm cadence stays shorter than the TTL,
// or the slot expires between passes. It checks the relationship, so either number can be tuned.
func TestPrewarmIssuersWarmsInsideTheCacheTTL(t *testing.T) {
	const minHeadroom = 2 // survive a dropped cycle
	if lightCadence*minHeadroom >= issuersCacheTTL {
		t.Fatalf("prewarm cadence %v gives less than %dx headroom against the %v "+
			"issuers cache TTL — a single dropped cycle would expose a cold slot",
			lightCadence, minHeadroom, issuersCacheTTL)
	}
}

// TestPrewarmIssuersNilReaderIsSafe — a deployment with no issuers
// reader wired must not panic the prewarm goroutine, which runs
// detached and would take the whole cadence down with it.
func TestPrewarmIssuersNilReaderIsSafe(t *testing.T) {
	prewarmIssuers(context.Background(), discardLogger(), nil)
}
