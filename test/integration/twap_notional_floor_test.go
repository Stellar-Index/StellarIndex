//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// Migration 0166 (migrations/0166_twap_notional_floor.up.sql) puts 0115's
// $0.01 notional floor on the TWAP chain. TWAP is equal-weight twice over —
// per trade in prices_1m, per minute in twap_1h / twap_1d — so before 0166 a
// 2-stroop crumb at an absurd price counted as much as a $1,000 fill, and a
// minute holding only that crumb counted as much as a minute of real trading.
//
// Every scenario stores a real fill at 5 and dust at 500 (or 0.5), so the
// unfloored answers (252.5, 502.5) cannot be mistaken for the floored 5.

const (
	twapFloorIssuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

	real5    = "50000000" // quote for base 10000000 → price 5
	realBase = "10000000"
	realUSD  = "1000"
	dustUSD  = "0.0000003"
)

// The operator re-materialisation recipe from 0166's header, run verbatim:
// windowed, forced, prices_1m before the TWAP views built on it.
var twapFloorRefreshRecipe = []string{
	"CALL refresh_continuous_aggregate('prices_1m', now() - INTERVAL '7 days', now(), force => true)",
	"CALL refresh_continuous_aggregate('twap_1h', now() - INTERVAL '7 days', now(), force => true)",
	"CALL refresh_continuous_aggregate('twap_1d', now() - INTERVAL '7 days', now(), force => true)",
}

func TestTWAPNotionalFloor(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// One hour into a fully closed UTC day inside the recipe's 7-day window,
	// so the minute, hour and day buckets all materialise and serve.
	t0 := time.Now().UTC().Add(-48 * time.Hour).Truncate(24 * time.Hour).Add(time.Hour)
	pair := func(code string) ohlcDustPair {
		return ohlcDustPair{base: code + "-" + twapFloorIssuer, quote: "native"}
	}

	// Mixed minute: a real fill and a crumb in the SAME minute.
	mixed := pair("TWMX")
	seed(t, db, ctx, mixed, []seedTrade{
		{off: 10 * time.Second, base: realBase, quote: real5, usd: realUSD},
		{off: 20 * time.Second, base: "2", quote: "1000", usd: dustUSD}, // 500
	}, t0)

	// Thin hour: one real minute, one minute holding only a crumb.
	thin := pair("TWTH")
	seed(t, db, ctx, thin, []seedTrade{
		{off: 10 * time.Second, base: realBase, quote: real5, usd: realUSD},
		{off: 2 * time.Minute, base: "2", quote: "1000", usd: dustUSD}, // 500
	}, t0)

	// All-dust hour: nothing clears the floor, so the fallback must report.
	allDust := pair("TWDU")
	seed(t, db, ctx, allDust, []seedTrade{
		{off: 10 * time.Second, base: "2", quote: "1000", usd: dustUSD}, // 500
		{off: 2 * time.Minute, base: "2", quote: "1", usd: "0.0000001"}, // 0.5
	}, t0)

	// Unpriced pair (usd_volume NULL): behaviour unchanged.
	unpriced := pair("TWNU")
	seed(t, db, ctx, unpriced, []seedTrade{
		{off: 10 * time.Second, base: realBase, quote: real5, usd: ""},
		{off: 2 * time.Minute, base: "2", quote: "1000", usd: ""},
	}, t0)

	// Two stored directions: a real forward minute, and a dust-only minute
	// stored REVERSE (native/TWDM at 0.001, i.e. 1000 once oriented).
	merged := pair("TWDM")
	seed(t, db, ctx, merged, []seedTrade{
		{off: 10 * time.Second, base: realBase, quote: real5, usd: realUSD},
	}, t0)
	seed(t, db, ctx, ohlcDustPair{base: "native", quote: merged.base}, []seedTrade{
		{off: 2 * time.Minute, base: realBase, quote: "10000", usd: "0.000001"},
	}, t0)

	for _, stmt := range twapFloorRefreshRecipe {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	t.Run("prices_1m twap ignores the crumb in a mixed minute", func(t *testing.T) {
		assertNumeric(t, "prices_1m.twap", readMinuteTWAP(t, db, ctx, mixed, t0), "5")
		var notional, trades int64
		if err := db.QueryRowContext(ctx, `
			SELECT notional_trade_count, trade_count FROM prices_1m
			 WHERE base_asset = $1 AND quote_asset = $2 AND bucket = $3`,
			mixed.base, mixed.quote, t0).Scan(&notional, &trades); err != nil {
			t.Fatalf("read prices_1m counts: %v", err)
		}
		if notional != 1 || trades != 2 {
			t.Errorf("notional_trade_count, trade_count = %d, %d; want 1, 2 — the floor "+
				"filters the twap only, never the trade count", notional, trades)
		}
	})

	for _, view := range []string{"twap_1h", "twap_1d"} {
		t.Run(view+" drops the dust-only minute", func(t *testing.T) {
			got := readTWAPRow(t, db, ctx, view, thin, t0)
			assertNumeric(t, view+".twap", got.twap, "5")
			if got.sampleCount != 1 {
				t.Errorf("sample_count = %d, want 1 — it must count exactly the minutes the twap averaged",
					got.sampleCount)
			}
			if n := readNotionalSamples(t, db, ctx, view, thin, t0); n != 1 {
				t.Errorf("notional_sample_count = %d, want 1", n)
			}
		})
		t.Run(view+" all-dust falls back rather than going NULL", func(t *testing.T) {
			got := readTWAPRow(t, db, ctx, view, allDust, t0)
			assertNumeric(t, view+".twap", got.twap, "250.25")
			if got.sampleCount != 2 {
				t.Errorf("sample_count = %d, want 2", got.sampleCount)
			}
			if n := readNotionalSamples(t, db, ctx, view, allDust, t0); n != 0 {
				t.Errorf("notional_sample_count = %d, want 0", n)
			}
		})
		t.Run(view+" unpriced pair is unchanged", func(t *testing.T) {
			got := readTWAPRow(t, db, ctx, view, unpriced, t0)
			assertNumeric(t, view+".twap", got.twap, "252.5")
			if got.sampleCount != 2 {
				t.Errorf("sample_count = %d, want 2", got.sampleCount)
			}
		})
	}

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	asset, err := c.NewClassicAsset("TWDM", twapFloorIssuer)
	if err != nil {
		t.Fatal(err)
	}
	p, err := c.NewPair(asset, c.NativeAsset())
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range []timescale.HistoryGranularity{timescale.Granularity1h, timescale.Granularity1d} {
		t.Run("served "+string(g)+" TWAP drops the fallback direction", func(t *testing.T) {
			pts, err := store.TWAPPointsInRange(ctx, p, g, t0.Add(-2*time.Hour), t0.Add(24*time.Hour), 0)
			if err != nil {
				t.Fatalf("TWAPPointsInRange: %v", err)
			}
			if len(pts) != 1 {
				t.Fatalf("got %d points, want 1", len(pts))
			}
			assertNumeric(t, "served twap", sql.NullString{String: pts[0].VWAP, Valid: true}, "5")
		})
	}
}

type twapRow struct {
	twap        sql.NullString
	sampleCount int64
}

func readMinuteTWAP(t *testing.T, db *sql.DB, ctx context.Context, p ohlcDustPair, bucket time.Time) sql.NullString {
	t.Helper()
	var got sql.NullString
	if err := db.QueryRowContext(ctx, `
		SELECT twap::text FROM prices_1m
		 WHERE base_asset = $1 AND quote_asset = $2 AND bucket = $3`,
		p.base, p.quote, bucket).Scan(&got); err != nil {
		t.Fatalf("read prices_1m.twap for %s: %v", p.base, err)
	}
	return got
}

func readTWAPRow(t *testing.T, db *sql.DB, ctx context.Context, view string, p ohlcDustPair, t0 time.Time) twapRow {
	t.Helper()
	var got twapRow
	if err := db.QueryRowContext(ctx, `
		SELECT twap::text, sample_count FROM `+view+`
		 WHERE base_asset = $1 AND quote_asset = $2 AND bucket <= $3
		 ORDER BY bucket DESC LIMIT 1`,
		p.base, p.quote, t0).Scan(&got.twap, &got.sampleCount); err != nil {
		t.Fatalf("read %s for %s: %v", view, p.base, err)
	}
	return got
}

func readNotionalSamples(t *testing.T, db *sql.DB, ctx context.Context, view string, p ohlcDustPair, t0 time.Time) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRowContext(ctx, `
		SELECT notional_sample_count FROM `+view+`
		 WHERE base_asset = $1 AND quote_asset = $2 AND bucket <= $3
		 ORDER BY bucket DESC LIMIT 1`,
		p.base, p.quote, t0).Scan(&n); err != nil {
		t.Fatalf("read %s.notional_sample_count for %s: %v", view, p.base, err)
	}
	return n
}
