//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestTimedVWAPsForPair1m_NotionalFloor pins that the anomaly baseline's
// series (and so the bootstrap cap's density) counts a minute only when a
// trade in it cleared the $0.01 notional floor (#1108). All amounts 1e7.
//
//	m0  one $20 fill                                   → counts
//	m1  one $0.001 dust fill                           → no
//	m2  one unpriced fill (usd_volume NULL)            → no
//	m3  $0.001 dust beside a $20 fill                  → counts
//	m4  $0.001 dust stored; $20 fill stored flipped    → counts (direction union)
//	m5..m64  sixty dust-only minutes                   → none, whatever the count
func TestTimedVWAPsForPair1m_NotionalFloor(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	db := store.DB()

	usdcID := "USDC-" + zeroLegIssuer
	thin := ohlcDustPair{base: "THIN-" + zeroLegIssuer, quote: usdcID}
	flip := ohlcDustPair{base: usdcID, quote: thin.base}

	t0 := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Hour)
	m := func(i int) time.Time { return t0.Add(time.Duration(i) * time.Minute) }
	real := seedTrade{off: 10 * time.Second, base: "100000000", quote: "200000000", usd: "20"}
	dust := seedTrade{off: 20 * time.Second, base: "5", quote: "10", usd: "0.001"}

	seed(t, db, ctx, thin, []seedTrade{real}, m(0))
	seed(t, db, ctx, thin, []seedTrade{dust}, m(1))
	seed(t, db, ctx, thin, []seedTrade{{off: 10 * time.Second, base: "100000000", quote: "200000000"}}, m(2))
	seed(t, db, ctx, thin, []seedTrade{dust, real}, m(3))
	seed(t, db, ctx, thin, []seedTrade{dust}, m(4))
	seed(t, db, ctx, flip, []seedTrade{{off: 30 * time.Second, base: "200000000", quote: "100000000", usd: "20"}}, m(4))
	for i := 5; i < 65; i++ {
		seed(t, db, ctx, thin, []seedTrade{dust}, m(i))
	}
	if _, err := db.ExecContext(ctx, "CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)"); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}

	usdc, err := c.NewClassicAsset("USDC", zeroLegIssuer)
	if err != nil {
		t.Fatal(err)
	}
	thinAsset, err := c.NewClassicAsset("THIN", zeroLegIssuer)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := c.NewPair(thinAsset, usdc)
	if err != nil {
		t.Fatal(err)
	}

	pts, err := store.TimedVWAPsForPair1m(ctx, pair, t0, m(65))
	if err != nil {
		t.Fatalf("TimedVWAPsForPair1m: %v", err)
	}
	want := []time.Time{m(1), m(4), m(5)} // bucket ends of m0, m3, m4
	if len(pts) != len(want) {
		t.Fatalf("points = %+v, want bucket ends %v", pts, want)
	}
	for i, p := range pts {
		if !p.BucketEnd.Equal(want[i]) {
			t.Errorf("point %d bucket end = %s, want %s", i, p.BucketEnd, want[i])
		}
		if p.VWAP < 1.99 || p.VWAP > 2.01 {
			t.Errorf("point %d vwap = %v, want ~2", i, p.VWAP)
		}
	}

	dustOnly, err := store.TimedVWAPsForPair1m(ctx, pair, m(5), m(65))
	if err != nil {
		t.Fatalf("TimedVWAPsForPair1m dust window: %v", err)
	}
	if len(dustOnly) != 0 {
		t.Errorf("dust-only window returned %d points, want 0", len(dustOnly))
	}
}
