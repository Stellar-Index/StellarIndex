//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestPairMarket_BothDirectionsAndWindowBoundaries pins the SERVED values
// of the /v1/pairs single-pair summary against real TimescaleDB, across
// the three boundaries the 2026-09-03 query rewrite moved:
//
//   - both stored orientations (the SDEX decoder records XLM/USDC and
//     USDC/XLM as separate rows) fold into ONE row for the requested
//     orientation — count_24h sums across directions;
//   - count_24h counts the trailing 24 hours and nothing older, even
//     though the pair is active across the whole 14-day recency window;
//   - last_trade_at is the exact newest trade INSIDE MarketsRecencyWindow
//     — a trade older than the window neither sets it nor makes an
//     otherwise-dormant pair visible.
//
// Honesty note on what this test is and is not. It is NOT the redness
// proof for the rewrite: the old and new queries are value-identical by
// construction (verified set-identical over 40 sampled live pairs on r1,
// 2026-09-03), because the defect was the PLAN, not the answer. The
// redness proof is TestPairMarketQueryShape in internal/storage/timescale,
// which fails on the pre-fix query text. This test exists because the
// rewrite split one 14-day aggregate into four independently-bounded
// reads, and the cheapest way for a future edit to make /v1/pairs faster
// still is to narrow one of those bounds too far — dropping a direction
// halves count_24h, and pulling the 14-day probe back to 24 hours makes
// every pair idle for a day vanish from the endpoint. Both would be
// silent, and both fail here.
func TestPairMarket_BothDirectionsAndWindowBoundaries(t *testing.T) {
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
	xlm, err := c.ParseAsset("crypto:XLM")
	if err != nil {
		t.Fatal(err)
	}
	fwd, err := c.NewPair(xlm, usdc)
	if err != nil {
		t.Fatal(err)
	}
	rev, err := c.NewPair(usdc, xlm)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	// newest is the last trade inside the recency window.
	newest := now.Add(-90 * time.Second)

	seed := []struct {
		nonce int
		ts    time.Time
		pair  c.Pair
	}{
		// Inside 24h — 4 forward, 2 flipped. count_24h must be 6.
		{1, newest, fwd},
		{2, now.Add(-2 * time.Hour), fwd},
		{3, now.Add(-9 * time.Hour), fwd},
		{4, now.Add(-23 * time.Hour), fwd},
		{5, now.Add(-30 * time.Minute), rev},
		{6, now.Add(-20 * time.Hour), rev},
		// Inside the 14d recency window but OUTSIDE 24h — these keep the
		// pair visible but must NOT be counted.
		{7, now.Add(-2 * 24 * time.Hour), fwd},
		{8, now.Add(-5 * 24 * time.Hour), fwd},
		{9, now.Add(-13 * 24 * time.Hour), rev},
		// Outside the 14d recency window entirely.
		{10, now.Add(-20 * 24 * time.Hour), fwd},
	}
	for _, s := range seed {
		if err := store.InsertTrade(ctx, mkAPITrade(s.nonce, s.ts, s.pair, 1_000_000_000, 12_000_000)); err != nil {
			t.Fatalf("InsertTrade[%d]: %v", s.nonce, err)
		}
	}

	m, found, err := store.PairMarket(ctx, xlm, usdc)
	if err != nil {
		t.Fatalf("PairMarket: %v", err)
	}
	if !found {
		t.Fatal("PairMarket reported the pair as untraded; 9 trades sit inside " +
			"MarketsRecencyWindow across both orientations")
	}
	if got := m.LastTradeAt.UTC(); !got.Equal(newest) {
		t.Errorf("last_trade_at = %s, want %s (the exact newest trade inside the "+
			"recency window, second-precision — /v1/pairs does NOT round to a CAGG "+
			"bucket the way /v1/markets does)", got, newest)
	}
	if m.TradeCount24h != 6 {
		t.Errorf("count_24h = %d, want 6 (4 forward + 2 flipped inside 24h). A "+
			"count of 4 means the flipped orientation was dropped; a count of 9 "+
			"means the 24-hour bound was widened to the recency window",
			m.TradeCount24h)
	}
	if m.Pair.Base.String() != xlm.String() || m.Pair.Quote.String() != usdc.String() {
		t.Errorf("pair = %s/%s, want the REQUESTED orientation %s/%s",
			m.Pair.Base, m.Pair.Quote, xlm, usdc)
	}

	// The reciprocal request folds the same two orientations and reports
	// the same activity, echoed in ITS requested orientation.
	rm, rfound, err := store.PairMarket(ctx, usdc, xlm)
	if err != nil {
		t.Fatalf("PairMarket reciprocal: %v", err)
	}
	if !rfound || rm.TradeCount24h != 6 || !rm.LastTradeAt.UTC().Equal(newest) {
		t.Errorf("reciprocal orientation disagrees: found=%v count_24h=%d last=%s; "+
			"want true/6/%s — both directions must fold the same way whichever way "+
			"round the caller asks", rfound, rm.TradeCount24h, rm.LastTradeAt.UTC(), newest)
	}

	// A pair whose ONLY trade is older than MarketsRecencyWindow stays
	// invisible: the 14-day bound on the last_trade_at probes is the
	// recency gate, and dropping it to 24h (the cheapest way to make this
	// query faster still) would hide every pair idle for a day.
	aqua, err := c.NewClassicAsset("AQUA", "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA")
	if err != nil {
		t.Fatal(err)
	}
	dormant, err := c.NewPair(aqua, usdc)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.InsertTrade(ctx, mkAPITrade(11, now.Add(-20*24*time.Hour), dormant, 1_000_000_000, 12_000_000)); err != nil {
		t.Fatalf("InsertTrade dormant: %v", err)
	}
	if _, ok, err := store.PairMarket(ctx, aqua, usdc); err != nil {
		t.Fatalf("PairMarket dormant: %v", err)
	} else if ok {
		t.Error("a pair whose only trade predates MarketsRecencyWindow was reported " +
			"as traded; /v1/pairs must stay consistent with DistinctPairs")
	}

	// And a pair idle for 3 days but active inside the window IS visible,
	// with count_24h = 0 rather than a miss.
	idle, err := c.NewPair(usdc, aqua)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.InsertTrade(ctx, mkAPITrade(12, now.Add(-3*24*time.Hour), idle, 1_000_000_000, 12_000_000)); err != nil {
		t.Fatalf("InsertTrade idle: %v", err)
	}
	im, ifound, err := store.PairMarket(ctx, usdc, aqua)
	if err != nil {
		t.Fatalf("PairMarket idle: %v", err)
	}
	if !ifound {
		t.Fatal("a pair active 3 days ago is inside MarketsRecencyWindow and must " +
			"still be served")
	}
	if im.TradeCount24h != 0 {
		t.Errorf("idle pair count_24h = %d, want 0", im.TradeCount24h)
	}
}
