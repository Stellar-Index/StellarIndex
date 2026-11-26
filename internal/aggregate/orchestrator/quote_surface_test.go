package orchestrator

import (
	"math/big"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// surfaceEnd is a closed bucket end, hour-aligned like every production window end.
var surfaceEnd = time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)

// surfaceTick is one instant's prints: each surface's price and base volume (nil base = no print).
type surfaceTick struct {
	proxyPrice, ownPrice *big.Rat
	proxyBase, ownBase   *big.Int
}

// buildSurface prints both surfaces every step across [surfaceEnd-window, surfaceEnd): the proxy at the step start, the own-quote
// print half a step later, priced and sized by at(offset from window start).
func buildSurface(
	t *testing.T, window, step time.Duration, at func(offset time.Duration) surfaceTick,
) ([]canonical.Trade, map[string]struct{}) {
	t.Helper()
	xlmUSD := mkPair(t, "crypto", "XLM", "fiat", "USD")
	start := surfaceEnd.Add(-window)
	var trades []canonical.Trade
	proxied := map[string]struct{}{}
	for off := time.Duration(0); off < window; off += step {
		tk := at(off)
		if tk.proxyBase != nil {
			tr := surfacePrint(xlmUSD, start.Add(off), tk.proxyBase, tk.proxyPrice)
			trades = append(trades, tr)
			proxied[tr.ID()] = struct{}{}
		}
		if tk.ownBase != nil {
			trades = append(trades, surfacePrint(xlmUSD, start.Add(off+step/2), tk.ownBase, tk.ownPrice))
		}
	}
	return trades, proxied
}

func surfacePrint(pair canonical.Pair, ts time.Time, base *big.Int, price *big.Rat) canonical.Trade {
	quote := new(big.Int).Mul(base, price.Num())
	quote.Quo(quote, price.Denom())
	tr := pivotTrade(pair, "sdex", 1, 1, ts)
	tr.BaseAmount, tr.QuoteAmount = canonical.NewAmount(base), canonical.NewAmount(quote)
	return tr
}

// ramp is XLM at 0.12 for the first two thirds of a 24 h window, then rising linearly 6 % over the last 8 h.
func ramp(off time.Duration) *big.Rat {
	p := big.NewRat(12, 100)
	if rise := off - 16*time.Hour; rise > 0 {
		frac := new(big.Rat).SetFrac64(int64(rise), int64(8*time.Hour))
		p.Mul(p, frac.Mul(frac, big.NewRat(6, 100)).Add(frac, big.NewRat(1, 1)))
	}
	return p
}

func flat(time.Duration) *big.Rat { return big.NewRat(12, 100) }

// basis scales a price by (1 + bps/10_000).
func basis(p *big.Rat, bps int64) *big.Rat {
	return new(big.Rat).Mul(p, big.NewRat(10_000+bps, 10_000))
}

func xlm(n int64) *big.Int { return new(big.Int).Mul(big.NewInt(n), big.NewInt(10_000_000)) }

// readSurface judges trades the way a published leg is judged and returns the leg reading.
func readSurface(t *testing.T, window time.Duration, trades []canonical.Trade, proxied map[string]struct{}) legRef {
	t.Helper()
	cache, _ := newTestRedis(t)
	o := New(nil, cache, Config{Windows: []time.Duration{window}})
	return o.newLegRef(mkPair(t, "crypto", "XLM", "fiat", "USD"), window, big.NewRat(12, 100), trades, proxied)
}

func gapBps(lr legRef) float64 { return ratBps(lr.surfaceGap) }

// P1: the production false positive. Price rises 6 % in the last 8 h, stablecoin volume trades early and real-USD volume late, both at
// the same instantaneous price: no de-peg, so the leg must stand.
func TestQuoteSurface_TrendWithShiftedVolumeIsNotADepeg(t *testing.T) {
	trades, proxied := buildSurface(t, 24*time.Hour, 10*time.Minute, func(off time.Duration) surfaceTick {
		pb, ob := xlm(1000), xlm(50)
		if off >= 16*time.Hour {
			pb, ob = xlm(50), xlm(1000)
		}
		return surfaceTick{proxyPrice: ramp(off), ownPrice: ramp(off), proxyBase: pb, ownBase: ob}
	})
	lr := readSurface(t, 24*time.Hour, trades, proxied)
	if why := lr.quoteSurfaceRefusal(75); why != "" {
		t.Fatalf("refusal = %q (gap %.1f bps, overlap %d, aligned %v), want none: both surfaces print the same price at every instant",
			why, gapBps(lr), lr.surfaceOverlap, lr.surfaceAligned)
	}
	if g := gapBps(lr); g < -10 || g > 10 || !lr.surfaceAligned || lr.surfaceOverlap != 24 {
		t.Errorf("gap %.1f bps aligned=%v overlap=%d, want |gap| < 10 bps aligned over all 24 sub-buckets", g, lr.surfaceAligned, lr.surfaceOverlap)
	}
}

// P2: an 80 bps basis at every instant is a de-peg whether the price is flat or trending.
func TestQuoteSurface_PersistentBasisIsRefused(t *testing.T) {
	for name, price := range map[string]func(time.Duration) *big.Rat{"flat": flat, "trending": ramp} {
		trades, proxied := buildSurface(t, 24*time.Hour, 10*time.Minute, func(off time.Duration) surfaceTick {
			pb, ob := xlm(1000), xlm(50)
			if off >= 16*time.Hour {
				pb, ob = xlm(50), xlm(1000)
			}
			return surfaceTick{proxyPrice: basis(price(off), 80), ownPrice: price(off), proxyBase: pb, ownBase: ob}
		})
		lr := readSurface(t, 24*time.Hour, trades, proxied)
		if lr.quoteSurfaceRefusal(75) == "" {
			t.Errorf("%s: not refused (gap %.1f bps), want an 80 bps basis refused", name, gapBps(lr))
		}
		if g := gapBps(lr); g < 78 || g > 82 {
			t.Errorf("%s: gap %.1f bps, want ~+80", name, g)
		}
	}
}

// depegLast prints both surfaces equally at a flat price, the proxy 100 bps rich for the newest `last` of the window.
func depegLast(t *testing.T, window, step, last time.Duration) legRef {
	t.Helper()
	trades, proxied := buildSurface(t, window, step, func(off time.Duration) surfaceTick {
		proxy := flat(off)
		if off >= window-last {
			proxy = basis(proxy, 100)
		}
		return surfaceTick{proxyPrice: proxy, ownPrice: flat(off), proxyBase: xlm(100), ownBase: xlm(100)}
	})
	return readSurface(t, window, trades, proxied)
}

// P3: a de-peg that begins late is caught by the newest overlapping sub-buckets, not out-voted by the older window: detection takes
// two sub-buckets (24h: 2 h, 1h: 4 m, 5m: 2 m).
func TestQuoteSurface_DepegInProgressIsCaughtWithinTwoSubBuckets(t *testing.T) {
	cases := []struct {
		window, step, last time.Duration
		refused            bool
	}{
		{24 * time.Hour, 10 * time.Minute, 6 * time.Hour, true},
		{24 * time.Hour, 10 * time.Minute, 2 * time.Hour, true},
		{24 * time.Hour, 10 * time.Minute, time.Hour, false},
		{time.Hour, 20 * time.Second, 4 * time.Minute, true},
		{time.Hour, 20 * time.Second, 2 * time.Minute, false},
		{5 * time.Minute, 10 * time.Second, 2 * time.Minute, true},
	}
	for _, tc := range cases {
		lr := depegLast(t, tc.window, tc.step, tc.last)
		if got := lr.quoteSurfaceRefusal(75) != ""; got != tc.refused {
			t.Errorf("window %v, de-peg over the last %v: refused=%v (gap %.1f bps), want %v", tc.window, tc.last, got, gapBps(lr), tc.refused)
		}
	}
}

// P4: one surface empty is not measured (pivot_unverified upstream), exactly as before alignment.
func TestQuoteSurface_OneSideEmptyIsUnmeasured(t *testing.T) {
	trades, proxied := buildSurface(t, time.Hour, time.Minute, func(off time.Duration) surfaceTick {
		return surfaceTick{proxyPrice: basis(ramp(off), 300), proxyBase: xlm(10)}
	})
	lr := readSurface(t, time.Hour, trades, proxied)
	if lr.surfaceGap != nil || lr.surfaceDivergence != nil || lr.surfaceUncomputable || lr.proxyShare != 1 {
		t.Errorf("all-proxy leg: gap=%v divergence=%v uncomputable=%v share=%v, want unmeasured with share 1",
			lr.surfaceGap, lr.surfaceDivergence, lr.surfaceUncomputable, lr.proxyShare)
	}
	if lr = readSurface(t, time.Hour, trades, nil); lr.surfaceGap != nil || lr.proxyShare != 0 {
		t.Errorf("no proxy prints: gap=%v share=%v, want unmeasured with share 0", lr.surfaceGap, lr.proxyShare)
	}
}

// P5: too little overlap to align falls back to the whole-window compare, which can refuse a trend but never passes a de-peg.
func TestQuoteSurface_ThinOverlapFallsBackToWholeWindow(t *testing.T) {
	// Three shared sub-buckets, but they hold well under half of the proxy volume.
	trades, proxied := buildSurface(t, 24*time.Hour, 10*time.Minute, func(off time.Duration) surfaceTick {
		if off >= 21*time.Hour {
			return surfaceTick{proxyPrice: ramp(off), ownPrice: ramp(off), proxyBase: xlm(10), ownBase: xlm(1000)}
		}
		return surfaceTick{proxyPrice: ramp(off), proxyBase: xlm(1000)}
	})
	lr := readSurface(t, 24*time.Hour, trades, proxied)
	if lr.surfaceAligned || lr.surfaceOverlap != 3 {
		t.Fatalf("aligned=%v overlap=%d, want the whole-window fallback with overlap 3", lr.surfaceAligned, lr.surfaceOverlap)
	}
	if lr.quoteSurfaceRefusal(75) == "" {
		t.Errorf("gap %.1f bps not refused, want the whole-window compare to refuse what it cannot align", gapBps(lr))
	}
}

// P6: the reading is a function of the trade set, not its order.
func TestQuoteSurface_DeterministicUnderShuffle(t *testing.T) {
	trades, proxied := buildSurface(t, 24*time.Hour, 10*time.Minute, func(off time.Duration) surfaceTick {
		proxy := ramp(off)
		if off >= 18*time.Hour {
			proxy = basis(proxy, 100+int64(off/time.Hour))
		}
		return surfaceTick{proxyPrice: proxy, ownPrice: ramp(off), proxyBase: xlm(100 + int64(off/time.Hour)), ownBase: xlm(100)}
	})
	want := readSurface(t, 24*time.Hour, trades, proxied)
	rng := rand.New(rand.NewPCG(1, 2))
	for i := range 20 {
		shuffled := append([]canonical.Trade(nil), trades...)
		rng.Shuffle(len(shuffled), func(a, b int) { shuffled[a], shuffled[b] = shuffled[b], shuffled[a] })
		got := readSurface(t, 24*time.Hour, shuffled, proxied)
		if got.surfaceGap.Cmp(want.surfaceGap) != 0 || got.surfaceOverlap != want.surfaceOverlap {
			t.Fatalf("shuffle %d: gap %s overlap %d, want %s overlap %d", i,
				got.surfaceGap.RatString(), got.surfaceOverlap, want.surfaceGap.RatString(), want.surfaceOverlap)
		}
	}
}

// P7: single-print sub-buckets above 2^100 stay exact: an exact 0.8 % basis reads exactly 1/125.
func TestQuoteSurface_ExactAboveTwoToTheHundred(t *testing.T) {
	huge := new(big.Int).Lsh(big.NewInt(1), 101)
	trades, proxied := buildSurface(t, 5*time.Minute, time.Minute, func(off time.Duration) surfaceTick {
		base := new(big.Int).Add(huge, big.NewInt(int64(off/time.Minute)))
		base.Mul(base, big.NewInt(1_000_000))
		return surfaceTick{proxyPrice: basis(flat(off), 80), ownPrice: flat(off), proxyBase: base, ownBase: base}
	})
	lr := readSurface(t, 5*time.Minute, trades, proxied)
	if lr.surfaceGap == nil || lr.surfaceGap.Cmp(big.NewRat(1, 125)) != 0 || !lr.surfaceAligned {
		t.Fatalf("gap = %v aligned=%v, want exactly 1/125 aligned", lr.surfaceGap, lr.surfaceAligned)
	}
	if lr.quoteSurfaceRefusal(75) == "" {
		t.Error("an exact 80 bps basis was not refused")
	}
}

// The judged leg's signed gap reaches stellarindex_aggregator_quote_surface_bps under the target's (pair, window, leg).
func TestQuoteSurface_GaugeCarriesTheSignedGap(t *testing.T) {
	xlmUSD := mkPair(t, "crypto", "XLM", "fiat", "USD")
	xlmGBP := mkPair(t, "crypto", "XLM", "fiat", "GBP")
	window := 24 * time.Hour
	cache, _ := newTestRedis(t)
	o := New(nil, cache, Config{Windows: []time.Duration{window}})
	o.setTickLegRef(xlmUSD, window, legRef{
		price: big.NewRat(12, 100), sources: 2, surfaceGap: big.NewRat(-80, 10_000),
		surfaceDivergence: big.NewRat(80, 10_000), surfaceAligned: true, surfaceOverlap: 20,
	})
	ref := compositeReference{
		legSources: map[string]int{}, legDispersionBps: map[string]float64{},
		legSurfaceBps: map[string]float64{}, legSurface: map[string]string{},
	}
	ref.recordLegReadings(xlmUSD, o.tickLegRefs[window])
	o.emitCompositeReference(xlmGBP, window, ref)
	got := testutil.ToFloat64(obs.AggregatorQuoteSurfaceBps.WithLabelValues(xlmGBP.String(), windowLabel(window), xlmUSD.String()))
	if got < -80.01 || got > -79.99 {
		t.Errorf("quote_surface_bps = %v, want -80", got)
	}
	if s := ref.legSurfaceString(); s != "{crypto:XLM/fiat:USD:-80.0bps/aligned=20}" {
		t.Errorf("leg_quote_surface = %q", s)
	}
	o.clearCompositeReference(xlmGBP, window)
	if obs.AggregatorQuoteSurfaceBps.DeleteLabelValues(xlmGBP.String(), windowLabel(window), xlmUSD.String()) {
		t.Error("quote_surface_bps series survived clearCompositeReference")
	}
}
