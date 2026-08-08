//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/usage"
)

// TestUsageDailyRollupReSweepIdempotent pins the billing-adjacent
// invariant behind GET /v1/account/usage: the 5-minute usage-rollup
// worker (internal/usage.Rollup.Sweep) hands the sink the FULL
// CUMULATIVE per-(day, subject, endpoint) counters on EVERY sweep — it
// deliberately never resets or checkpoints the Redis source. That is
// only safe because timescale.Store.UpsertUsageDaily merges with
// GREATEST(existing, incoming), NOT additively. If that merge were ever
// flipped to `col = usage_daily.col + EXCLUDED.col`, every 5-minute
// sweep would RE-ADD the cumulative value and usage_daily would report
// k*N requests for N real requests — a permanent, non-self-correcting
// over-count on the surface a metered plan bills against (audit
// W2-plat-1 / candidate b).
//
// The unit suite (internal/usage/rollup_test.go) only proves the sweep
// hands the same cumulative batch on replay, then delegates: "idempotence
// is then the sink's GREATEST()-merge contract." Nothing exercised that
// contract against real Postgres until this test — so a regression that
// broke it would ship green. This drives the REAL SQL and asserts N, not
// kN. Flip either GREATEST to `+` in usage_daily.go and this goes red.
func TestUsageDailyRollupReSweepIdempotent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// day is now-relative: ReadUsageDaily reads a trailing now-anchored
	// window, so a hardcoded date is a calendar time-bomb — the original
	// "2026-07-29" literal aged out of the 7-day window on 2026-08-05 and
	// the test started failing untouched (ci-health flood, 2026-08-08).
	day := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	const subj = "key:kid_bill_1"

	// One day of real traffic to /v1/price for one subject: 100 ok, 5 4xx,
	// 2 5xx, 3 throttled. These are the CUMULATIVE per-day counters the
	// Redis detail hash holds at sweep time.
	batch := []usage.RollupRow{{
		Day: day, Subject: subj, Endpoint: "/v1/price",
		OK: 100, ClientErrors: 5, ServerErrors: 2, Throttled: 3,
	}}

	// The worker sweeps the SAME cumulative batch every 5 minutes with no
	// new traffic in between. Six sweeps == 30 minutes of re-folding.
	for i := 0; i < 6; i++ {
		if err := store.UpsertUsageDaily(ctx, batch); err != nil {
			t.Fatalf("sweep %d UpsertUsageDaily: %v", i, err)
		}
	}

	rows, err := store.ReadUsageDaily(ctx, subj, 7)
	if err != nil {
		t.Fatalf("ReadUsageDaily: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ReadUsageDaily returned %d rows, want 1 (one endpoint, one day): %+v", len(rows), rows)
	}
	got := rows[0]
	// EXACTLY the single-sweep values — NOT 6x. An additive merge would
	// yield ok=600, client=30, server=12, throttled=18 here.
	if got.OK != 100 {
		t.Errorf("ok_count = %d after 6 sweeps, want 100 (additive over-count would give 600)", got.OK)
	}
	if got.ClientErrors != 5 {
		t.Errorf("client_error_count = %d, want 5 (additive would give 30)", got.ClientErrors)
	}
	if got.ServerErrors != 2 {
		t.Errorf("server_error_count = %d, want 2 (additive would give 12)", got.ServerErrors)
	}
	if got.Throttled != 3 {
		t.Errorf("throttled_count = %d, want 3 (additive would give 18)", got.Throttled)
	}
	// The wire-shape total /v1/account/usage serves: requests = ok + 4xx + 5xx.
	if reqs := got.OK + got.ClientErrors + got.ServerErrors; reqs != 107 {
		t.Errorf("wire requests = %d, want 107 (billing over-count if higher)", reqs)
	}
}

// TestUsageDailyRollupWithinDayGrowth pins the other half of the
// GREATEST contract: WITHIN a day the Redis counters only grow, so each
// sweep's cumulative value is >= the last. GREATEST must keep the LATEST
// (largest) value, never the sum of the partial snapshots. A sweep at
// t=5m sees 50 ok; a sweep at t=10m sees the full 100 ok. The persisted
// row must read 100 — additive would read 150.
func TestUsageDailyRollupWithinDayGrowth(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	day := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	const subj = "key:kid_bill_2"

	// Sweep 1: half a day's traffic captured.
	if err := store.UpsertUsageDaily(ctx, []usage.RollupRow{{
		Day: day, Subject: subj, Endpoint: "/v1/assets/{asset_id}", OK: 50,
	}}); err != nil {
		t.Fatalf("sweep 1: %v", err)
	}
	// Sweep 2: the day's full cumulative total (the counter grew to 100).
	if err := store.UpsertUsageDaily(ctx, []usage.RollupRow{{
		Day: day, Subject: subj, Endpoint: "/v1/assets/{asset_id}", OK: 100,
	}}); err != nil {
		t.Fatalf("sweep 2: %v", err)
	}
	// Sweep 3: a late-arriving Redis snapshot that is SMALLER (e.g. the
	// detail hash partially expired, or a mid-day flush). GREATEST must
	// hold the row at 100 and never regress to 40.
	if err := store.UpsertUsageDaily(ctx, []usage.RollupRow{{
		Day: day, Subject: subj, Endpoint: "/v1/assets/{asset_id}", OK: 40,
	}}); err != nil {
		t.Fatalf("sweep 3: %v", err)
	}

	rows, err := store.ReadUsageDaily(ctx, subj, 7)
	if err != nil {
		t.Fatalf("ReadUsageDaily: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ReadUsageDaily returned %d rows, want 1: %+v", len(rows), rows)
	}
	if got := rows[0].OK; got != 100 {
		t.Errorf("ok_count = %d, want 100 (GREATEST of 50/100/40; additive would give 190, regress would give 40)", got)
	}
}
