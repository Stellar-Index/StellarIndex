//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestPairMarketSubstanceAt_MeasuresTheMarketAtTheInstant executes the
// point-in-time substance SQL against real TimescaleDB, and then runs
// the thin-market gate over it end to end (finding T038).
//
// The defect: /v1/price/at and the /v1/price/changes horizons serve the
// bucket at-or-before a past `ts`, but the gate deciding whether to
// serve it measured the trailing 24h ending NOW. The two are unrelated,
// and the fixture holds one market for each direction that got wrong:
//
//   - DEEP/native  — deep and honest 400 days ago, no trades since.
//     The trailing gate withholds its entire history.
//   - SEED/native  — one $8.57 burst 400 days ago, thick today. The
//     trailing gate PASSES it, so the historical read serves the seeded
//     price.
//
// Two decoy trades sit just outside the historical window on either
// side (one bucket before it opens; the still-open bucket at the
// instant itself), so an off-by-one on either literal bound changes the
// counted legs and fails the exact assertions.
func TestPairMarketSubstanceAt_MeasuresTheMarketAtTheInstant(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	deep, err := c.NewClassicAsset("DEEP", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	seed, err := c.NewClassicAsset("SEED", "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA")
	if err != nil {
		t.Fatal(err)
	}
	deepXLM, _ := c.NewPair(deep, c.NativeAsset())
	xlmDeep, _ := c.NewPair(c.NativeAsset(), deep) // the flipped stored direction
	seedXLM, _ := c.NewPair(seed, c.NativeAsset())

	now := time.Now().UTC().Truncate(time.Minute)
	then := now.Add(-400 * 24 * time.Hour).Truncate(time.Hour)

	var trades []c.Trade
	nonce := 0
	add := func(ts time.Time, pair c.Pair) {
		nonce++
		trades = append(trades, mkAPITrade(nonce, ts, pair, 1_000_000, 500_000))
	}
	// DEEP, historical: 24 trades, one every 20 min from then−9h to
	// then−1h20m, alternating stored direction → 8 distinct hour buckets
	// (then−9h … then−2h), span 7h.
	for i := 0; i < 24; i++ {
		pair := deepXLM
		if i%2 == 1 {
			pair = xlmDeep
		}
		add(then.Add(-9*time.Hour+time.Duration(i)*20*time.Minute), pair)
	}
	add(then.Add(-25*time.Hour), deepXLM)  // decoy: one bucket BEFORE the window opens
	add(then.Add(30*time.Minute), deepXLM) // decoy: the bucket still OPEN at the instant
	add(then.Add(-3*time.Hour), seedXLM)   // SEED, historical: a single burst
	for i := 0; i < 24; i++ {              // SEED, today: 24 minutes, now−10h … now−2h20m
		add(now.Add(-10*time.Hour+time.Duration(i)*20*time.Minute), seedXLM)
	}
	for _, tr := range trades {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade: %v", err)
		}
	}
	// Stamp the dollar leg directly: what is under test is the window,
	// not the insert-time valuation rules.
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE trades SET usd_volume = 100 WHERE source = 'integ-api'`); err != nil {
		t.Fatalf("stamp usd_volume: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE trades SET usd_volume = 8.57 WHERE source = 'integ-api' AND base_asset = $1 AND ts < $2::timestamptz`,
		seed.String(), now.Add(-24*time.Hour)); err != nil {
		t.Fatalf("stamp seed usd_volume: %v", err)
	}
	for _, view := range []string{"prices_1m", "prices_1h"} {
		if _, err := store.DB().ExecContext(ctx,
			`CALL refresh_continuous_aggregate('`+view+`', NULL, NULL)`); err != nil {
			t.Fatalf("refresh %s: %v", view, err)
		}
	}

	day := 24 * time.Hour
	measure := func(pair c.Pair, asOf time.Time, g timescale.HistoryGranularity) timescale.MarketSubstance {
		t.Helper()
		sub, err := store.PairMarketSubstanceAt(ctx, pair, asOf, day, g)
		if err != nil {
			t.Fatalf("PairMarketSubstanceAt(%s, %s, %s): %v", pair, asOf, g, err)
		}
		return sub
	}
	wantSub := func(name string, got timescale.MarketSubstance, volume float64, buckets, span int64) {
		t.Helper()
		if v := mustFloat(t, got.VolumeUSD); v != volume || got.Buckets != buckets || got.SpanSeconds != span {
			t.Errorf("%s: got {volume %s, buckets %d, span %ds}, want {volume %v, buckets %d, span %ds}",
				name, got.VolumeUSD, got.Buckets, got.SpanSeconds, volume, buckets, span)
		}
	}

	// ── the SQL: the window sits at the instant ────────────────────────
	wantSub("DEEP at then, hour grain — both directions, decoys excluded",
		measure(deepXLM, then, timescale.Granularity1h), 2400, 8, 7*3600)
	wantSub("DEEP at then, asked in the flipped orientation",
		measure(xlmDeep, then, timescale.Granularity1h), 2400, 8, 7*3600)
	wantSub("SEED at then, hour grain — the single burst",
		measure(seedXLM, then, timescale.Granularity1h), 8.57, 1, 0)
	wantSub("SEED at now−1h, minute grain — today's market",
		measure(seedXLM, now.Add(-time.Hour), timescale.Granularity1m), 2400, 24, 460*60)
	// Moving the instant moves the window: at now−5h only the trades up
	// to now−5h−1m had closed (i ≤ 14 → 15 minutes, span 280 min).
	wantSub("SEED at now−5h, minute grain — the window moved with the instant",
		measure(seedXLM, now.Add(-5*time.Hour), timescale.Granularity1m), 1500, 15, 280*60)
	wantSub("DEEP at now, minute grain — dormant today",
		measure(deepXLM, now, timescale.Granularity1m), 0, 0, 0)

	// The trailing reader agrees with the point-in-time one AT now — the
	// new query is the old one with the window moved, nothing else.
	live, err := store.PairMarketSubstance(ctx, seedXLM, day)
	if err != nil {
		t.Fatalf("PairMarketSubstance: %v", err)
	}
	wantSub("SEED trailing-from-now", live, 2400, 24, 460*60)

	// ── the gate, end to end over the real store ───────────────────────
	gate := pricingguard.NewSubstanceGate(store, pricingguard.SubstanceGateOptions{})
	if gate.Allowed(ctx, deep, c.NativeAsset(), "test") {
		t.Error("fixture: DEEP must be below the LIVE floor (no trades for 400 days)")
	}
	if !gate.AllowedAt(ctx, deep, c.NativeAsset(), then, "test") {
		t.Error("DEEP at `then` was withheld: its market cleared every leg of the floor at that " +
			"instant, and being dormant today is not a reason to refuse its history")
	}
	if !gate.Allowed(ctx, seed, c.NativeAsset(), "test") {
		t.Error("fixture: SEED must clear the LIVE floor today")
	}
	if gate.AllowedAt(ctx, seed, c.NativeAsset(), then, "test") {
		t.Error("SEED at `then` was served: its market at that instant was one $8.57 burst, and " +
			"being thick today is not a reason to publish the seeded price")
	}
	if !gate.AllowedAt(ctx, seed, c.NativeAsset(), now.Add(-time.Hour), "test") {
		t.Error("SEED at now−1h was withheld though its recent market clears the minute floor")
	}
}
