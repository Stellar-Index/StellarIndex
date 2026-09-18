//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestMarketsListingGrain pins the two properties the /v1/markets
// directory read is required to have and did not (F027 / K031 / F028).
// Both are executed against a real TimescaleDB with the real CAGGs, in
// the state r1 is actually in: prices_1d materialized only up to the
// PREVIOUS UTC midnight, which is what its 6-hour end_offset +
// materialized_only setting produce for every pair that traded today
// (migrations/0147, policy row '1d').
//
//  1. per-source figures are that SOURCE's. prices_1m / prices_1d are
//     grouped by (bucket, base, quote) with an
//     `array_agg(DISTINCT source) AS sources` column, so filtering them
//     by `$source = ANY(sources)` selects the buckets a venue printed
//     in and then sums EVERY venue's trades in those buckets. The
//     fixture below makes that visible: soroswap prints 2 trades worth
//     $10 into minutes in which sdex printed 6 worth $600.
//
//  2. last_price is the latest price, and a market that first traded
//     today is listed. Reading both from prices_1d served the previous
//     UTC day's close and dropped same-day-new markets entirely.
func TestMarketsListingGrain(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	db := store.DB()

	const usdc = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	now := time.Now().UTC()
	dayStart := now.Truncate(24 * time.Hour)

	// ── pair A: native/USDC on two venues, same minutes ───────────────
	// Two older prints, one per venue (3 days back: inside the 14d
	// recency window, outside the 24h figures window), so the pair has a
	// materialized prices_1d bucket whose `sources` array holds both
	// venues. Without them the pair-wide read would simply return no row
	// and the assertions below could not tell a cross-source sum from an
	// absent pair.
	seedGrainTrade(t, ctx, db, 1, "sdex", now.Add(-72*time.Hour), "native", usdc, "1", "0.5", "1")
	seedGrainTrade(t, ctx, db, 2, "soroswap", now.Add(-72*time.Hour), "native", usdc, "1", "0.5", "1")
	for i := range 6 {
		seedGrainTrade(t, ctx, db, 10+i, "sdex", now.Add(-30*time.Minute), "native", usdc, "1", "0.5", "100")
	}
	for i := range 2 {
		seedGrainTrade(t, ctx, db, 20+i, "soroswap", now.Add(-30*time.Minute), "native", usdc, "1", "0.5", "5")
	}

	// ── pair B: traded yesterday at 10, again today at 20 ─────────────
	seedGrainTrade(t, ctx, db, 30, "sdex", dayStart.Add(-6*time.Hour), "crypto:ETH", "fiat:USD", "1", "10", "10")
	seedGrainTrade(t, ctx, db, 31, "sdex", now.Add(-1*time.Minute), "crypto:ETH", "fiat:USD", "1", "20", "20")

	// ── pair C: first trade is TODAY ──────────────────────────────────
	seedGrainTrade(t, ctx, db, 40, "sdex", now.Add(-2*time.Minute), "crypto:SOL", "fiat:USD", "1", "7", "7")

	// prices_1m and pools_per_source_1h are materialized whole (their
	// end_offsets are 30 s and 5 min). prices_1d is materialized only up
	// to the previous UTC midnight — the state its own 6h end_offset
	// leaves it in for every pair that has traded today.
	for _, stmt := range []string{
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`,
		`CALL refresh_continuous_aggregate('pools_per_source_1h', NULL, NULL)`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("refresh cagg (%s): %v", stmt, err)
		}
	}
	if _, err := db.ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1d', NULL, $1::timestamptz)`, dayStart,
	); err != nil {
		t.Fatalf("refresh prices_1d up to %s: %v", dayStart, err)
	}

	t.Run("source figures are that source's own", func(t *testing.T) {
		markets, _, err := store.SourceMarkets(ctx, "soroswap", "", 100, timescale.MarketsOrderVolume24hDesc)
		if err != nil {
			t.Fatalf("SourceMarkets: %v", err)
		}
		row := findGrainMarket(t, markets, "native", usdc)
		if row.TradeCount24h != 2 {
			t.Errorf("SourceMarkets(soroswap).trade_count_24h = %d, want 2 (soroswap's own prints; sdex printed 6 more into the same minutes)",
				row.TradeCount24h)
		}
		if got := numeric(t, row.Volume24hUSD); got != 10 {
			t.Errorf("SourceMarkets(soroswap).volume_24h_usd = %v, want 10 (soroswap's own USD volume; the pair's cross-venue total is 610)", got)
		}

		// The same venue's same pair, from the surface that always had
		// the per-source grain. These two disagreed by orders of
		// magnitude; they are now the same CTE.
		pools, _, err := store.AllPools(ctx,
			timescale.PoolsFilter{Sources: []string{"soroswap"}}, "", 100, timescale.MarketsOrderVolume24hDesc)
		if err != nil {
			t.Fatalf("AllPools: %v", err)
		}
		if len(pools) != 1 {
			t.Fatalf("AllPools(soroswap) returned %d rows, want 1", len(pools))
		}
		if pools[0].TradeCount24h != row.TradeCount24h {
			t.Errorf("/v1/markets?source=soroswap trade_count_24h = %d but /v1/pools?source=soroswap = %d — the two surfaces must agree",
				row.TradeCount24h, pools[0].TradeCount24h)
		}
		if numeric(t, pools[0].Volume24hUSD) != numeric(t, row.Volume24hUSD) {
			t.Errorf("/v1/markets?source=soroswap volume_24h_usd = %v but /v1/pools?source=soroswap = %v — the two surfaces must agree",
				numeric(t, row.Volume24hUSD), numeric(t, pools[0].Volume24hUSD))
		}
	})

	t.Run("last_price is today's price, not yesterday's close", func(t *testing.T) {
		markets, _, err := store.DistinctPairsExt(ctx, "", 500, timescale.MarketsOrderPair)
		if err != nil {
			t.Fatalf("DistinctPairsExt: %v", err)
		}
		row := findGrainMarket(t, markets, "crypto:ETH", "fiat:USD")
		if got := numeric(t, row.LastPrice); got != 20 {
			t.Errorf("last_price = %v, want 20 (today's print; 10 is yesterday's prices_1d close)", got)
		}
	})

	t.Run("a market whose first trade is today is listed", func(t *testing.T) {
		markets, _, err := store.DistinctPairsExt(ctx, "", 500, timescale.MarketsOrderPair)
		if err != nil {
			t.Fatalf("DistinctPairsExt: %v", err)
		}
		row := findGrainMarket(t, markets, "crypto:SOL", "fiat:USD")
		if got := numeric(t, row.LastPrice); got != 7 {
			t.Errorf("last_price = %v, want 7", got)
		}
		if !row.BucketCloseAt.Equal(dayStart) {
			t.Errorf("bucket_close_at = %s, want %s (the day bucket it last traded in)",
				row.BucketCloseAt, dayStart)
		}
	})
}

// seedGrainTrade writes one trade straight to the hypertable so the
// fixture can set usd_volume (the pricing pipeline's output) without
// running the pipeline.
func seedGrainTrade(t *testing.T, ctx context.Context, db *sql.DB, nonce int, source string, ts time.Time, base, quote, baseAmt, quoteAmt, usdVolume string) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO trades
		    (source, ledger, tx_hash, op_index, ts,
		     base_asset, quote_asset, base_amount, quote_amount, usd_volume)
		VALUES ($1, $2, $3, 0, $4, $5, $6, $7::numeric, $8::numeric, $9::numeric)`,
		source, 70_000_000+nonce, fmt.Sprintf("%064x", 700_000+nonce), ts,
		base, quote, baseAmt, quoteAmt, usdVolume,
	); err != nil {
		t.Fatalf("seed trade %d (%s): %v", nonce, source, err)
	}
}

// findGrainMarket locates one pair in a listing regardless of which
// orientation the canonical collapse picked.
func findGrainMarket(t *testing.T, markets []timescale.Market, base, quote string) timescale.Market {
	t.Helper()
	for _, m := range markets {
		b, q := m.Pair.Base.String(), m.Pair.Quote.String()
		if (b == base && q == quote) || (b == quote && q == base) {
			return m
		}
	}
	got := make([]string, 0, len(markets))
	for _, m := range markets {
		got = append(got, m.Pair.Base.String()+"|"+m.Pair.Quote.String())
	}
	t.Fatalf("pair %s|%s is missing from the listing; got %v", base, quote, got)
	return timescale.Market{}
}

// numeric parses a wire-shaped numeric string for comparison. nil is
// reported as 0 so a missing figure fails the same assertion a wrong
// one does.
func numeric(t *testing.T, s *string) float64 {
	t.Helper()
	if s == nil {
		return 0
	}
	v, err := strconv.ParseFloat(*s, 64)
	if err != nil {
		t.Fatalf("un-parseable numeric %q: %v", *s, err)
	}
	return v
}
