//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestLatestClosedVWAP1m_RecentExistenceGate exercises the recent-existence
// gate added for the 2026-07-06 empty-alias latency incident. The
// /v1/price handler reads native/fiat:USD as an alias on every XLM query,
// and that synthetic pair has ZERO rows; before the gate, each such read
// ran a max() over ~400 days of prices_1m chunks to conclude "no rows"
// (minutes cold), timing out before the fast crypto:XLM/fiat:USD alias was
// tried. The gate makes the empty/quiet case an O(recent-chunks) probe
// that returns sql.ErrNoRows, while a live pair still returns its latest
// closed bucket combining BOTH stored directions.
//
// Three cases, one fixture:
//   - empty pair (native/fiat:USD, no rows) → sql.ErrNoRows,
//   - quiet pair whose only bucket predates the gate window → sql.ErrNoRows
//     (proving the GATE, not just "no rows", bounds the horizon: the
//     ~400-day value walk WOULD have found this bucket),
//   - live pair with a recent flipped-only latest bucket → the combined,
//     inverted VWAP (both directions), NOT a miss.
func TestLatestClosedVWAP1m_RecentExistenceGate(t *testing.T) {
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
	aqua, err := c.NewClassicAsset("AQUA", "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA")
	if err != nil {
		t.Fatal(err)
	}
	fiatUSD, err := c.ParseAsset("fiat:USD")
	if err != nil {
		t.Fatal(err)
	}

	xlmUSDC, _ := c.NewPair(c.NativeAsset(), usdc) // requested orientation
	usdcXLM, _ := c.NewPair(usdc, c.NativeAsset()) // the flipped storage direction
	xlmAQUA, _ := c.NewPair(c.NativeAsset(), aqua)
	xlmUSD, _ := c.NewPair(c.NativeAsset(), fiatUSD) // never traded — the incident pair

	// Live pair: two recent closed buckets ~2h back. The LATEST bucket
	// stores ONLY the flipped direction (2.0 XLM/USDC = 0.5 USDC/XLM once
	// inverted), so a one-direction read would either miss it or return the
	// older direct bucket — the combine + inversion is what makes the
	// answer correct.
	recent := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Minute)
	// Quiet pair: a single closed bucket 30 days back — inside the ~400-day
	// value window but OUTSIDE the 14-day gate window.
	quiet := time.Now().UTC().Add(-30 * 24 * time.Hour).Truncate(time.Minute)

	trades := []c.Trade{
		mkAPITrade(1, recent, xlmUSDC, 1_000_000, 500_000),                      // 0.5 direct
		mkAPITrade(2, recent.Add(2*time.Minute), usdcXLM, 1_000_000, 2_000_000), // 2.0 flipped → 0.5
		mkAPITrade(3, quiet, xlmAQUA, 1_000_000, 250_000),                       // quiet pair, 30d old
	}
	for _, tr := range trades {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade: %v", err)
		}
	}
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}

	// ── empty pair → ErrNoRows (no full walk) ──────────────────────────
	if _, err := store.LatestClosedVWAP1mForPair(ctx, xlmUSD); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("empty pair native/fiat:USD: err = %v, want sql.ErrNoRows", err)
	}

	// ── quiet pair (bucket older than the gate window) → ErrNoRows ─────
	// This is the load-bearing gate assertion: the bucket exists 30d back,
	// so the ~400-day value walk WOULD return it; the gate is what makes
	// the read fall through to the handler's last-trade fallback instead.
	if _, err := store.LatestClosedVWAP1mForPair(ctx, xlmAQUA); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("quiet pair native/AQUA (bucket 30d old, gate window 14d): err = %v, want sql.ErrNoRows", err)
	}

	// ── live pair → latest closed bucket, both directions combined ─────
	row, err := store.LatestClosedVWAP1mForPair(ctx, xlmUSDC)
	if err != nil {
		t.Fatalf("live pair native/USDC: unexpected err %v", err)
	}
	wantBucket := recent.Add(2 * time.Minute)
	if !row.Bucket.Equal(wantBucket) {
		t.Errorf("live pair latest bucket = %v, want %v (the flipped-only bucket — proves both directions read)",
			row.Bucket, wantBucket)
	}
	if px := mustFloat(t, row.VWAP); px < 0.49 || px > 0.51 {
		t.Errorf("live pair VWAP = %s, want ~0.5 (flipped 2.0 inverted — proves direction combine)", row.VWAP)
	}
}
