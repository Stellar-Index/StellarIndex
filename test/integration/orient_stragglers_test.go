//go:build integration

package integration_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	c "github.com/StellarIndex/stellar-index/internal/canonical"
	"github.com/StellarIndex/stellar-index/internal/storage/timescale"
)

// TestOrientStragglers_CombineBothDirections proves the rc.131 pair-direction
// canonicalization now reaches the straggler read paths (BACKLOG #55 / item 3):
// VWAPsForPair1m, PairMarket, and OHLCSeries each combine BOTH stored
// directions of a market into the requested orientation, inverting the flipped
// rows. The SDEX decoder records XLM/USDC and USDC/XLM as separate rows, so a
// per-direction read misses the flipped-only bucket entirely.
func TestOrientStragglers_CombineBothDirections(t *testing.T) {
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

	// Anchor two 1-minute buckets ~2h back so both are closed + inside 24h.
	t0 := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Minute)

	// Bucket A stores the requested direction at price 0.5 USDC/XLM.
	// Bucket B stores ONLY the flipped direction at price 2.0 XLM/USDC —
	// which is the SAME market at 0.5 USDC/XLM once inverted.
	trades := []c.Trade{
		mkAPITrade(1, t0, xlmUSDC, 1_000_000, 500_000),                      // price 0.5
		mkAPITrade(2, t0.Add(2*time.Minute), usdcXLM, 1_000_000, 2_000_000), // price 2.0 → 0.5 inverted
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

	from := t0.Add(-time.Minute)
	to := t0.Add(3 * time.Minute)

	// ── VWAPsForPair1m ─────────────────────────────────────────────
	vwaps, err := store.VWAPsForPair1m(ctx, xlmUSDC, from, to)
	if err != nil {
		t.Fatalf("VWAPsForPair1m: %v", err)
	}
	if len(vwaps) != 2 {
		t.Fatalf("VWAPsForPair1m returned %d buckets, want 2 (both directions combined): %v", len(vwaps), vwaps)
	}
	for i, v := range vwaps {
		if v < 0.49 || v > 0.51 {
			t.Errorf("VWAPsForPair1m[%d] = %v, want ~0.5 (flipped bucket inverted)", i, v)
		}
	}

	// ── PairMarket ─────────────────────────────────────────────────
	m, ok, err := store.PairMarket(ctx, c.NativeAsset(), usdc)
	if err != nil {
		t.Fatalf("PairMarket: %v", err)
	}
	if !ok {
		t.Fatal("PairMarket ok=false, want true")
	}
	if m.TradeCount24h != 2 {
		t.Errorf("PairMarket TradeCount24h = %d, want 2 (both directions)", m.TradeCount24h)
	}
	if m.LastPrice == nil {
		t.Fatal("PairMarket LastPrice = nil, want the inverted flipped-bucket price")
	}
	if lp := mustFloat(t, *m.LastPrice); lp < 0.49 || lp > 0.51 {
		t.Errorf("PairMarket LastPrice = %s, want ~0.5 (latest bucket is flipped, inverted)", *m.LastPrice)
	}

	// ── OHLCSeries ─────────────────────────────────────────────────
	bars, err := store.OHLCSeries(ctx, xlmUSDC, timescale.HistoryGranularity("1m"), from, to, 0)
	if err != nil {
		t.Fatalf("OHLCSeries: %v", err)
	}
	if len(bars) != 2 {
		t.Fatalf("OHLCSeries returned %d bars, want 2 (both directions combined): %+v", len(bars), bars)
	}
	// The second bar is the flipped-only bucket — its OHLC must be inverted.
	b := bars[1]
	for name, s := range map[string]string{"open": b.Open, "high": b.High, "low": b.Low, "close": b.Close} {
		if px := mustFloat(t, s); px < 0.49 || px > 0.51 {
			t.Errorf("OHLCSeries flipped bar %s = %s, want ~0.5 (inverted from 2.0)", name, s)
		}
	}
	if b.TradeCount != 1 {
		t.Errorf("OHLCSeries flipped bar TradeCount = %d, want 1", b.TradeCount)
	}

	// ── TimedVWAPsForPair1m (anomaly baseline) ─────────────────────
	timed, err := store.TimedVWAPsForPair1m(ctx, xlmUSDC, from, to)
	if err != nil {
		t.Fatalf("TimedVWAPsForPair1m: %v", err)
	}
	if len(timed) != 2 {
		t.Fatalf("TimedVWAPsForPair1m returned %d points, want 2 (both directions)", len(timed))
	}
	for i, tv := range timed {
		if tv.VWAP < 0.49 || tv.VWAP > 0.51 {
			t.Errorf("TimedVWAPsForPair1m[%d].VWAP = %v, want ~0.5", i, tv.VWAP)
		}
	}

	// ── OHLCSeriesReBucketed (5m fold) ─────────────────────────────
	// Both source buckets combine + invert first, then fold into the
	// coarser 5m grid. Assert on totals so the check is independent of
	// which 5m bucket each minute lands in.
	rb, err := store.OHLCSeriesReBucketed(ctx, xlmUSDC, timescale.HistoryGranularity("1m"), "5 minutes", from, to, 0)
	if err != nil {
		t.Fatalf("OHLCSeriesReBucketed: %v", err)
	}
	if len(rb) == 0 {
		t.Fatal("OHLCSeriesReBucketed returned no bars, want the flipped bucket combined in")
	}
	var rbTrades int64
	for _, bar := range rb {
		rbTrades += bar.TradeCount
		if px := mustFloat(t, bar.Close); px < 0.49 || px > 0.51 {
			t.Errorf("OHLCSeriesReBucketed bar Close = %s, want ~0.5 (inverted)", bar.Close)
		}
	}
	if rbTrades != 2 {
		t.Errorf("OHLCSeriesReBucketed total TradeCount = %d, want 2 (both directions)", rbTrades)
	}
}

func mustFloat(t *testing.T, s string) float64 {
	t.Helper()
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		t.Fatalf("ParseFloat(%q): %v", s, err)
	}
	return f
}
