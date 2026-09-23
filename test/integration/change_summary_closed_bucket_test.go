//go:build integration

package integration_test

import (
	"context"
	"math/big"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestTimedVWAPs1mForChangeSummary_ExcludesTheOpenBucket runs the
// /v1/changes source read against the real prices_1m CAGG (GH-757).
// The worker passes its wall clock as `to`, and `bucket < to` admitted the
// minute that clock sits inside: a fat-finger print there became
// current_value and, through the upsert's GREATEST/LEAST, the stored
// ath/atl for good.
//
// Deterministic on purpose: both buckets are minutes in the past and `to`
// is placed 30 s into the later one, so the later bucket is "in progress"
// relative to the caller without racing the real clock's minute edge.
func TestTimedVWAPs1mForChangeSummary_ExcludesTheOpenBucket(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = store.Close() }()

	usdc, err := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	pair, err := c.NewPair(c.NativeAsset(), usdc)
	if err != nil {
		t.Fatal(err)
	}

	closedBucket := time.Now().UTC().Truncate(time.Minute).Add(-10 * time.Minute)
	openBucket := closedBucket.Add(5 * time.Minute)
	// 100 XLM for 10 USDC: 0.1 — the honest closed price.
	if err := store.InsertTrade(ctx, mkIntegrationTrade("sdex", 1, closedBucket.Add(10*time.Second), pair,
		1_000_000_000, 100_000_000)); err != nil {
		t.Fatalf("InsertTrade closed: %v", err)
	}
	// 1 XLM for 1000 USDC: the fat-finger extreme in the unclosed minute.
	if err := store.InsertTrade(ctx, mkIntegrationTrade("sdex", 2, openBucket.Add(10*time.Second), pair,
		10_000_000, 10_000_000_000)); err != nil {
		t.Fatalf("InsertTrade open: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}

	to := openBucket.Add(30 * time.Second)
	got, err := store.TimedVWAPs1mForChangeSummary(ctx, pair, closedBucket.Add(-time.Hour), to)
	if err != nil {
		t.Fatalf("TimedVWAPs1mForChangeSummary: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("returned %d points %+v, want exactly the one closed bucket — "+
			"the bucket still open at `to` must not be served", len(got), got)
	}
	if want := closedBucket.Add(time.Minute); !got[0].At.Equal(want) {
		t.Errorf("point At = %s, want the closed bucket's end %s", got[0].At, want)
	}
	v, ok := new(big.Rat).SetString(got[0].Value)
	if !ok || v.Cmp(big.NewRat(1, 10)) != 0 {
		t.Errorf("point value = %q, want 0.1 (the closed bucket), not the open-minute extreme", got[0].Value)
	}

	// Once `to` is past it, the same bucket is closed and is served.
	got, err = store.TimedVWAPs1mForChangeSummary(ctx, pair, closedBucket.Add(-time.Hour), openBucket.Add(time.Minute))
	if err != nil {
		t.Fatalf("TimedVWAPs1mForChangeSummary (after close): %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("returned %d points after the later bucket closed, want 2", len(got))
	}
}
