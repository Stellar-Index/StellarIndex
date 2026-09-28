//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/baseline"
	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestTimedVWAPsForPair1m_NotionalFloor pins that the anomaly baseline's
// series carries each minute's USD notional across both stored directions,
// and that the refresher's USD-volume bars over it on real Postgres keep
// penny-authored minutes from buying baseline points (#1108). All amounts 1e7.
//
//	m0  one $20 fill                                   → $20
//	m1  one $0.001 dust fill                           → $0.001
//	m2  one unpriced fill (usd_volume NULL)            → nil
//	m3  $0.001 dust beside a $20 fill                  → $20.001
//	m4  $0.001 dust stored; $20 fill stored flipped    → $20.001
//	m5..m64  sixty $0.01 self-trade minutes            → $0.01 each
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
	eur := ohlcDustPair{base: "EURT-" + zeroLegIssuer, quote: usdcID}
	pny := ohlcDustPair{base: "PNY-" + zeroLegIssuer, quote: usdcID}

	t0 := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Hour)
	m := func(i int) time.Time { return t0.Add(time.Duration(i) * time.Minute) }
	real := seedTrade{off: 10 * time.Second, base: "100000000", quote: "200000000", usd: "20"}
	dust := seedTrade{off: 20 * time.Second, base: "5", quote: "10", usd: "0.001"}
	penny := seedTrade{off: 20 * time.Second, base: "50000", quote: "100000", usd: "0.01"}
	unpriced := seedTrade{off: 10 * time.Second, base: "100000000", quote: "200000000"}

	seed(t, db, ctx, thin, []seedTrade{real}, m(0))
	seed(t, db, ctx, thin, []seedTrade{dust}, m(1))
	seed(t, db, ctx, thin, []seedTrade{unpriced}, m(2))
	seed(t, db, ctx, thin, []seedTrade{dust, real}, m(3))
	seed(t, db, ctx, thin, []seedTrade{dust}, m(4))
	seed(t, db, ctx, flip, []seedTrade{{off: 30 * time.Second, base: "200000000", quote: "100000000", usd: "20"}}, m(4))
	for i := 5; i < 65; i++ {
		seed(t, db, ctx, thin, []seedTrade{penny}, m(i))
		seed(t, db, ctx, eur, []seedTrade{unpriced}, m(i))
		seed(t, db, ctx, pny, []seedTrade{penny}, m(i))
	}
	if _, err := db.ExecContext(ctx, "CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)"); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}

	usdc, err := c.NewClassicAsset("USDC", zeroLegIssuer)
	if err != nil {
		t.Fatal(err)
	}
	pairOf := func(code string) c.Pair {
		a, err := c.NewClassicAsset(code, zeroLegIssuer)
		if err != nil {
			t.Fatal(err)
		}
		p, err := c.NewPair(a, usdc)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	pair := pairOf("THIN")

	pts, err := store.TimedVWAPsForPair1m(ctx, pair, t0, m(65))
	if err != nil {
		t.Fatalf("TimedVWAPsForPair1m: %v", err)
	}
	if len(pts) != 65 {
		t.Fatalf("got %d points, want 65 (one per traded minute)", len(pts))
	}
	wants := map[int]string{0: "20", 1: "1/1000", 2: "", 3: "20001/1000", 4: "20001/1000"} // "" = nil
	for i := 5; i < 65; i++ {
		wants[i] = "1/100"
	}
	for i, p := range pts {
		if !p.BucketEnd.Equal(m(i + 1)) {
			t.Errorf("point %d bucket end = %s, want %s", i, p.BucketEnd, m(i+1))
		}
		if p.VWAP < 1.99 || p.VWAP > 2.01 {
			t.Errorf("point %d vwap = %v, want ~2", i, p.VWAP)
		}
		w := wants[i]
		switch {
		case w == "" && p.USDVolume != nil:
			t.Errorf("point %d usd = %s, want nil (unpriced)", i, p.USDVolume.RatString())
		case w != "" && (p.USDVolume == nil || p.USDVolume.RatString() != w):
			t.Errorf("point %d usd = %v, want %s", i, p.USDVolume, w)
		}
	}

	// The production refresher, floor as wired on defaults, over the same
	// rows: THIN's three real minutes are three points (N=2) and its sixty
	// pennies ($0.60) never make a point; a pair of nothing but pennies has
	// too little flow for bars and keeps main's per-minute baseline; the
	// all-unpriced pair keeps one under ok_unvalued.
	sink := &captureBaselineSink{}
	r := baseline.NewRefresher(store, sink, baseline.DefaultWindow, nil).
		WithMinuteNotionalFloor(baseline.MinuteNotionalFloor(10_000, 24*time.Hour))
	outcome, err := r.RefreshPair(ctx, pair)
	if err != nil || outcome != baseline.OutcomeOK || sink.last.Day30 == nil || sink.last.Day30.N != 2 {
		t.Errorf("THIN refresh = (%v, %v, %+v), want (ok, nil) with Day30.N=2", outcome, err, sink.last.Day30)
	}
	outcome, err = r.RefreshPair(ctx, pairOf("PNY"))
	if err != nil || outcome != baseline.OutcomeOKPerMinuteFallback || sink.last.Day30 == nil || sink.last.Day30.N != 59 {
		t.Errorf("PNY refresh = (%v, %v, %+v), want ok_per_minute_fallback with Day30.N=59", outcome, err, sink.last.Day30)
	}
	outcome, err = r.RefreshPair(ctx, pairOf("EURT"))
	if err != nil || outcome != baseline.OutcomeOKUnvalued {
		t.Errorf("EURT refresh = (%v, %v), want (ok_unvalued, nil)", outcome, err)
	}
	if sink.calls != 3 || sink.last.Day30 == nil || sink.last.Day30.N != 59 {
		t.Errorf("sink calls=%d last=%+v, want THIN, PNY, EURT upserts, EURT Day30.N=59", sink.calls, sink.last.Day30)
	}
}

type captureBaselineSink struct {
	calls int
	last  baseline.MultiBaseline
}

func (s *captureBaselineSink) UpsertBaseline(_ context.Context, _ c.Pair, _, _, _ time.Time, m baseline.MultiBaseline) error {
	s.calls++
	s.last = m
	return nil
}
