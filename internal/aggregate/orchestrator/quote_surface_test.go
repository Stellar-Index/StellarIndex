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

// surfaceEnd is a closed bucket end. Production ends are minute-aligned; tests that need partial edge slots end at surfaceOddEnd.
var (
	surfaceEnd    = time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)
	surfaceOddEnd = surfaceEnd.Add(37 * time.Minute)
)

// ownVenues print the own-quote (real USD) volume, split evenly; extraVenue is a third own-quote venue (thin or hostile).
var ownVenues = []string{"coinbase", "kraken"}

const (
	proxyVenue = "binance"
	extraVenue = "bitstamp"
)

// surfaceTick is one instant's prints (nil base = no print): the proxy, the own-quote volume split across ownVenues (or printed by the
// first alone when soleOwn), and an optional extraVenue print.
type surfaceTick struct {
	proxyPrice, ownPrice, extraPrice *big.Rat
	proxyBase, ownBase, extraBase    *big.Int
	soleOwn                          bool
}

// buildSurface prints every step across [surfaceEnd-window, surfaceEnd): the proxy at the step start, own-quote prints half a step
// later, priced and sized by at(offset from window start).
func buildSurface(
	t *testing.T, window, step time.Duration, at func(offset time.Duration) surfaceTick,
) ([]canonical.Trade, map[string]struct{}) {
	t.Helper()
	return buildSurfaceEnding(t, surfaceEnd, window, step, at)
}

// buildSurfaceEnding is buildSurface for a window ending at end.
func buildSurfaceEnding(
	t *testing.T, end time.Time, window, step time.Duration, at func(offset time.Duration) surfaceTick,
) ([]canonical.Trade, map[string]struct{}) {
	t.Helper()
	xlmUSD := mkPair(t, "crypto", "XLM", "fiat", "USD")
	start := end.Add(-window)
	var trades []canonical.Trade
	proxied := map[string]struct{}{}
	for off := time.Duration(0); off < window; off += step {
		tk := at(off)
		if tk.proxyBase != nil {
			tr := surfacePrint(xlmUSD, proxyVenue, start.Add(off), tk.proxyBase, tk.proxyPrice)
			trades = append(trades, tr)
			proxied[tr.ID()] = struct{}{}
		}
		ts := start.Add(off + step/2)
		if tk.ownBase != nil && tk.soleOwn {
			trades = append(trades, surfacePrint(xlmUSD, ownVenues[0], ts, tk.ownBase, tk.ownPrice))
		} else if tk.ownBase != nil {
			half := new(big.Int).Rsh(tk.ownBase, 1)
			trades = append(trades,
				surfacePrint(xlmUSD, ownVenues[0], ts, half, tk.ownPrice),
				surfacePrint(xlmUSD, ownVenues[1], ts, new(big.Int).Sub(tk.ownBase, half), tk.ownPrice))
		}
		if tk.extraBase != nil {
			trades = append(trades, surfacePrint(xlmUSD, extraVenue, ts, tk.extraBase, tk.extraPrice))
		}
	}
	return trades, proxied
}

func surfacePrint(pair canonical.Pair, venue string, ts time.Time, base *big.Int, price *big.Rat) canonical.Trade {
	quote := new(big.Int).Mul(base, price.Num())
	quote.Quo(quote, price.Denom())
	tr := pivotTrade(pair, venue, 1, 1, ts)
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

// xlm is n thousand XLM at the test venues' 8-decimal scale (at 0.12, 1 unit = 120 USD).
func xlm(n int64) *big.Int { return new(big.Int).Mul(big.NewInt(n), big.NewInt(100_000_000_000)) }

// shiftedVolume is the production shape: stablecoin volume early, real-USD volume late (4:1 each way, the thinnest own share an
// eligible slot may have).
func shiftedVolume(off time.Duration) (proxyBase, ownBase *big.Int) {
	if off >= 16*time.Hour {
		return xlm(250), xlm(1000)
	}
	return xlm(1000), xlm(250)
}

// readSurface judges trades the way a published leg is judged and returns the leg reading.
func readSurface(t *testing.T, window time.Duration, trades []canonical.Trade, proxied map[string]struct{}) legRef {
	t.Helper()
	cache, _ := newTestRedis(t)
	o := New(nil, cache, Config{Windows: []time.Duration{window}})
	return o.newLegRef(mkPair(t, "crypto", "XLM", "fiat", "USD"), window, big.NewRat(12, 100), trades, proxied)
}

func gapBps(lr legRef) float64 { return ratBps(lr.surfaceGap) }

// T1: the production false positive. Price rises 6 % in the last 8 h, stablecoin volume trades early and real-USD volume late, both at
// the same instantaneous price: the whole-window gap is ~-250 bps, but there is no de-peg, so the leg must stand.
func TestQuoteSurface_TrendWithShiftedVolumeIsNotADepeg(t *testing.T) {
	trades, proxied := buildSurface(t, 24*time.Hour, 10*time.Minute, func(off time.Duration) surfaceTick {
		pb, ob := shiftedVolume(off)
		return surfaceTick{proxyPrice: ramp(off), ownPrice: ramp(off), proxyBase: pb, ownBase: ob}
	})
	if whole := surfaceOracleOf(trades, proxied).whole; ratBps(whole) > -75 {
		t.Fatalf("whole-window gap %.1f bps; the case no longer reproduces the false positive", ratBps(whole))
	}
	lr := readSurface(t, 24*time.Hour, trades, proxied)
	if why := lr.quoteSurfaceRefusal(surfaceTol); why != "" {
		t.Fatalf("refusal = %q (gap %.1f bps, overlap %d, aligned %v), want none: both surfaces print the same price at every instant",
			why, gapBps(lr), lr.surfaceOverlap, lr.surfaceAligned)
	}
	if g := gapBps(lr); g < -10 || g > 10 || !lr.surfaceAligned || lr.surfaceOverlap != 24 || lr.surfaceCoverage.Cmp(big.NewRat(1, 1)) != 0 {
		t.Errorf("gap %.1f bps aligned=%v overlap=%d coverage=%v, want |gap| < 10 bps decided alone over all 24 slots at full coverage",
			g, lr.surfaceAligned, lr.surfaceOverlap, lr.surfaceCoverage)
	}
}

// T2: stablecoin prints 1 % below real USD at every instant is a de-peg whether the price is flat or trending.
func TestQuoteSurface_PersistentBasisIsRefused(t *testing.T) {
	for name, price := range map[string]func(time.Duration) *big.Rat{"flat": flat, "trending": ramp} {
		trades, proxied := buildSurface(t, 24*time.Hour, 10*time.Minute, func(off time.Duration) surfaceTick {
			pb, ob := shiftedVolume(off)
			return surfaceTick{proxyPrice: basis(price(off), -100), ownPrice: price(off), proxyBase: pb, ownBase: ob}
		})
		lr := readSurface(t, 24*time.Hour, trades, proxied)
		if lr.quoteSurfaceRefusal(surfaceTol) == "" {
			t.Errorf("%s: not refused (gap %.1f bps), want a -100 bps basis refused", name, gapBps(lr))
		}
		if g := gapBps(lr); g < -101 || g > -99 || !lr.surfaceAligned {
			t.Errorf("%s: gap %.1f bps aligned=%v, want ~-100 decided alone", name, g, lr.surfaceAligned)
		}
	}
}

// T3: a de-peg confined to part of the window is judged by its volume-weighted mean over the window's proxy prints, as the served leg
// carries it. By design (the whole-window check's blind spot too) a de-peg of size d over a share f of the proxy volume passes while
// d·f <= 75 bps: 3 % over the last third (100 bps) refuses, 3 % over the last 4 h (50 bps) and 1 % over the last 6 h (25 bps) do not.
func TestQuoteSurface_PartialDepegIsJudgedByItsWeightedMean(t *testing.T) {
	cases := []struct {
		window, step, last time.Duration
		bps                int64
		refused            bool
	}{
		{24 * time.Hour, 10 * time.Minute, 8 * time.Hour, -300, true},
		{24 * time.Hour, 10 * time.Minute, 4 * time.Hour, -300, false},
		{24 * time.Hour, 10 * time.Minute, 6 * time.Hour, 100, false},
		{time.Hour, 20 * time.Second, 20 * time.Minute, 300, true},
		{5 * time.Minute, 10 * time.Second, 2 * time.Minute, 300, true},
	}
	for _, tc := range cases {
		trades, proxied := buildSurface(t, tc.window, tc.step, func(off time.Duration) surfaceTick {
			proxy := ramp(off)
			if off >= tc.window-tc.last {
				proxy = basis(proxy, tc.bps)
			}
			return surfaceTick{proxyPrice: proxy, ownPrice: ramp(off), proxyBase: xlm(100), ownBase: xlm(100)}
		})
		lr := readSurface(t, tc.window, trades, proxied)
		if got := lr.quoteSurfaceRefusal(surfaceTol) != ""; got != tc.refused || !lr.surfaceAligned {
			t.Errorf("window %v, %+d bps over the last %v: refused=%v aligned=%v (gap %.1f bps), want refused=%v aligned",
				tc.window, tc.bps, tc.last, got, lr.surfaceAligned, gapBps(lr), tc.refused)
		}
	}
}

// T6: one own-quote venue per slot never sets the reference, so the reading is exactly the whole-window compare it replaced.
func TestQuoteSurface_SingleOwnVenueIsTheWholeWindowCompare(t *testing.T) {
	trades, proxied := buildSurface(t, 24*time.Hour, 10*time.Minute, func(off time.Duration) surfaceTick {
		pb, ob := shiftedVolume(off)
		return surfaceTick{proxyPrice: ramp(off), ownPrice: ramp(off), proxyBase: pb, ownBase: ob, soleOwn: true}
	})
	lr := readSurface(t, 24*time.Hour, trades, proxied)
	whole := surfaceOracleOf(trades, proxied).whole
	if lr.surfaceAligned || lr.surfaceOverlap != 0 || lr.surfaceGap == nil || lr.surfaceGap.Cmp(whole) != 0 {
		t.Fatalf("aligned=%v overlap=%d gap=%v, want the whole-window gap %s with no eligible slot",
			lr.surfaceAligned, lr.surfaceOverlap, lr.surfaceGap, whole.RatString())
	}
	if lr.quoteSurfaceRefusal(surfaceTol) == "" {
		t.Errorf("gap %.1f bps not refused, want the whole-window verdict", gapBps(lr))
	}
}

// A thin leg (both surfaces ~1 USD a print, under the 10k-per-hour floor) never decides alone: its verdict is exactly the
// whole-window compare's.
func TestQuoteSurface_ThinLegIsTheWholeWindowCompare(t *testing.T) {
	for name, bps := range map[string]int64{"trend, no de-peg": 0, "de-peg": -100} {
		trades, proxied := buildSurface(t, 24*time.Hour, 10*time.Minute, func(off time.Duration) surfaceTick {
			pb, ob := shiftedVolume(off)
			pb.Quo(pb, big.NewInt(100_000))
			ob.Quo(ob, big.NewInt(100_000))
			return surfaceTick{proxyPrice: basis(ramp(off), bps), ownPrice: ramp(off), proxyBase: pb, ownBase: ob}
		})
		lr := readSurface(t, 24*time.Hour, trades, proxied)
		whole := surfaceOracleOf(trades, proxied).whole
		if lr.surfaceAligned || lr.surfaceOverlap != 0 || lr.surfaceGap == nil || lr.surfaceGap.Cmp(whole) != 0 {
			t.Errorf("%s: aligned=%v overlap=%d gap=%v, want the whole-window gap %s with no eligible slot",
				name, lr.surfaceAligned, lr.surfaceOverlap, lr.surfaceGap, whole.RatString())
		}
		if got, want := lr.quoteSurfaceRefusal(surfaceTol) != "", exceeds(whole); got != want {
			t.Errorf("%s: refused=%v, want the whole-window verdict %v", name, got, want)
		}
	}
}

// One surface empty is not measured (pivot_unverified upstream).
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

// The reading is a function of the trade set, not its order.
func TestQuoteSurface_DeterministicUnderShuffle(t *testing.T) {
	trades, proxied := buildSurface(t, 24*time.Hour, 10*time.Minute, func(off time.Duration) surfaceTick {
		proxy := ramp(off)
		if off >= 18*time.Hour {
			proxy = basis(proxy, 100+int64(off/time.Hour))
		}
		return surfaceTick{
			proxyPrice: proxy, ownPrice: ramp(off), proxyBase: xlm(100 + int64(off/time.Hour)), ownBase: xlm(100),
			extraPrice: basis(ramp(off), 30), extraBase: xlm(7),
		}
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

// Single-print slots above 2^100 stay exact: an exact 0.8 % basis reads exactly 1/125.
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
	if lr.quoteSurfaceRefusal(surfaceTol) == "" {
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
		surfaceDivergence: big.NewRat(80, 10_000), surfaceAligned: true, surfaceOverlap: 20, surfaceCoverage: big.NewRat(1, 1),
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
	if s := ref.legSurfaceString(); s != "{crypto:XLM/fiat:USD:-80.0bps/aligned=20,cover=100.0%}" {
		t.Errorf("leg_quote_surface = %q", s)
	}
	o.clearCompositeReference(xlmGBP, window)
	if obs.AggregatorQuoteSurfaceBps.DeleteLabelValues(xlmGBP.String(), windowLabel(window), xlmUSD.String()) {
		t.Error("quote_surface_bps series survived clearCompositeReference")
	}
}
