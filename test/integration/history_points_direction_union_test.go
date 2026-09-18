//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestHistoryPointsDirectionUnion executes the two CAGG series reads
// behind /v1/history/since-inception and /v1/chart against a real
// TimescaleDB after they were rewritten from a both-directions OR
// disjunction into a UNION ALL of two single-direction branches with a
// sargable closed-bucket bound.
//
// The shape itself is pinned without a database by the scanning guard in
// internal/storage/timescale/prices_1m_direction_union_test.go. What
// only a live database can prove is that the rewritten statements still
// PARSE, still bind their parameters in the right order once the
// optional from/to/limit clauses move inside the branches, and still
// serve the same numbers: both stored orientations folded into the
// requested one, chronological order, the bucket LIMIT counting BUCKETS
// (not rows) across the two branches, the in-progress bucket excluded,
// and an unknown pair answered with an empty series rather than an
// error.
func TestHistoryPointsDirectionUnion(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	usdc, err := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	xlmUSDC, _ := c.NewPair(c.NativeAsset(), usdc) // requested orientation
	usdcXLM, _ := c.NewPair(usdc, c.NativeAsset()) // the flipped storage direction

	// Three closed 1-minute buckets ~2h back. The middle one is stored
	// ONLY in the flipped orientation — a one-direction read drops it,
	// and a UNION ALL that lost a branch would drop it too.
	t0 := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Minute)
	trades := []c.Trade{
		mkAPITrade(41, t0, xlmUSDC, 1_000_000, 500_000),                      // 0.5
		mkAPITrade(42, t0.Add(2*time.Minute), usdcXLM, 1_000_000, 2_000_000), // 2.0 → 0.5 inverted
		mkAPITrade(43, t0.Add(4*time.Minute), xlmUSDC, 1_000_000, 500_000),   // 0.5
	}
	for _, tr := range trades {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade: %v", err)
		}
	}
	for _, stmt := range []string{
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`,
		`CALL refresh_continuous_aggregate('prices_1d', NULL, NULL)`,
	} {
		if _, err := store.DB().ExecContext(ctx, stmt); err != nil {
			t.Fatalf("refresh cagg: %v", err)
		}
	}

	wantBuckets := []time.Time{t0, t0.Add(2 * time.Minute), t0.Add(4 * time.Minute)}

	assertSeries := func(t *testing.T, what string, pts []timescale.HistoryPoint, want []time.Time) {
		t.Helper()
		if len(pts) != len(want) {
			t.Fatalf("%s returned %d buckets, want %d: %+v", what, len(pts), len(want), pts)
		}
		for i, p := range pts {
			if !p.Bucket.UTC().Equal(want[i]) {
				t.Errorf("%s[%d].Bucket = %s, want %s (chronological, oldest first)",
					what, i, p.Bucket.UTC(), want[i])
			}
			// Every bucket is the same market at 0.5 USDC per XLM; the
			// flipped-only bucket must arrive INVERTED from its stored 2.0.
			if px := mustFloat(t, p.VWAP); px < 0.49 || px > 0.51 {
				t.Errorf("%s[%d].VWAP = %s, want ~0.5 (both directions folded into the requested orientation)",
					what, i, p.VWAP)
			}
		}
	}

	// ── HistoryPoints: the since-inception read (no lower bound) ────
	pts, err := store.HistoryPoints(ctx, xlmUSDC, timescale.Granularity1m, 0)
	if err != nil {
		t.Fatalf("HistoryPoints: %v", err)
	}
	assertSeries(t, "HistoryPoints", pts, wantBuckets)

	// The bucket LIMIT counts BUCKETS across both branches: with the cap
	// now applied per branch AND on the union, the first n buckets of the
	// merged series must still be complete and in order.
	for n := 1; n <= 3; n++ {
		capped, err := store.HistoryPoints(ctx, xlmUSDC, timescale.Granularity1m, n)
		if err != nil {
			t.Fatalf("HistoryPoints(limit=%d): %v", n, err)
		}
		assertSeries(t, "HistoryPoints(limit)", capped, wantBuckets[:n])
	}

	// Requesting the market the other way round serves the reciprocal —
	// the same rows, folded into the flipped orientation (1/0.5 = 2.0).
	rev, err := store.HistoryPoints(ctx, usdcXLM, timescale.Granularity1m, 0)
	if err != nil {
		t.Fatalf("HistoryPoints(reversed): %v", err)
	}
	if len(rev) != 3 {
		t.Fatalf("HistoryPoints(reversed) returned %d buckets, want 3", len(rev))
	}
	for i, p := range rev {
		if px := mustFloat(t, p.VWAP); px < 1.99 || px > 2.01 {
			t.Errorf("HistoryPoints(reversed)[%d].VWAP = %s, want ~2.0", i, p.VWAP)
		}
	}

	// An unknown-but-well-formed pair is the DoS scenario this rewrite
	// exists for: it must answer an empty series, not an error.
	eur, err := c.NewFiatAsset("EUR")
	if err != nil {
		t.Fatal(err)
	}
	emptyPair, _ := c.NewPair(c.NativeAsset(), eur)
	empty, err := store.HistoryPoints(ctx, emptyPair, timescale.Granularity1m, 0)
	if err != nil {
		t.Fatalf("HistoryPoints(empty pair): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("HistoryPoints(empty pair) returned %d buckets, want 0", len(empty))
	}

	// ── Closed-bucket guard, in its rewritten sargable spelling ─────
	// The 1-day bucket holding these trades is still IN PROGRESS, so the
	// series must be empty even though the CAGG row exists.
	var dayRows int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM prices_1d
		  WHERE (base_asset = $1 AND quote_asset = $2)
		     OR (base_asset = $2 AND quote_asset = $1)`,
		xlmUSDC.Base.String(), xlmUSDC.Quote.String(),
	).Scan(&dayRows); err != nil {
		t.Fatalf("count prices_1d: %v", err)
	}
	if dayRows == 0 {
		t.Fatal("prices_1d holds no row for the seeded pair — the closed-bucket " +
			"assertion below would pass vacuously")
	}
	dayPts, err := store.HistoryPoints(ctx, xlmUSDC, timescale.Granularity1d, 0)
	if err != nil {
		t.Fatalf("HistoryPoints(1d): %v", err)
	}
	if len(dayPts) != 0 {
		t.Errorf("HistoryPoints(1d) returned %d buckets, want 0 — today's bucket is "+
			"still open and ADR-0015 excludes it (`bucket <= now() - INTERVAL '1 day'`): %+v",
			len(dayPts), dayPts)
	}

	// ── HistoryPointsInRange: /v1/chart's windowed read ─────────────
	from := t0.Add(-time.Minute)
	to := t0.Add(5 * time.Minute)
	ranged, err := store.HistoryPointsInRange(ctx, xlmUSDC, timescale.Granularity1m, from, to, 0)
	if err != nil {
		t.Fatalf("HistoryPointsInRange: %v", err)
	}
	assertSeries(t, "HistoryPointsInRange", ranged, wantBuckets)

	// The window must still bite once the bounds live inside the
	// branches: [t0+1m, t0+3m) keeps only the flipped-only bucket.
	narrow, err := store.HistoryPointsInRange(ctx, xlmUSDC, timescale.Granularity1m,
		t0.Add(time.Minute), t0.Add(3*time.Minute), 0)
	if err != nil {
		t.Fatalf("HistoryPointsInRange(narrow): %v", err)
	}
	assertSeries(t, "HistoryPointsInRange(narrow)", narrow, wantBuckets[1:2])

	// Optional bounds: zero `from` means since-inception, zero `to`
	// means open-ended — both branches must still carry the rest of the
	// predicate list and the placeholders must stay in step.
	noFrom, err := store.HistoryPointsInRange(ctx, xlmUSDC, timescale.Granularity1m,
		time.Time{}, to, 0)
	if err != nil {
		t.Fatalf("HistoryPointsInRange(no from): %v", err)
	}
	assertSeries(t, "HistoryPointsInRange(no from)", noFrom, wantBuckets)

	noTo, err := store.HistoryPointsInRange(ctx, xlmUSDC, timescale.Granularity1m,
		from, time.Time{}, 1)
	if err != nil {
		t.Fatalf("HistoryPointsInRange(no to, limit 1): %v", err)
	}
	assertSeries(t, "HistoryPointsInRange(no to, limit 1)", noTo, wantBuckets[:1])
}
