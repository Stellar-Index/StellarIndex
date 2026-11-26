package orchestrator

import (
	"math/big"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

const surfaceTol = 75

// A de-peg in the hours the own-quote venues are silent is served through the whole-window VWAP, so the aligned reading (which only
// sees shared slots) must not replace it.
func TestQuoteSurface_QuietTailDepegIsRefused(t *testing.T) {
	trades, proxied := buildSurface(t, 24*time.Hour, 10*time.Minute, func(off time.Duration) surfaceTick {
		if off >= 18*time.Hour {
			return surfaceTick{proxyPrice: basis(flat(off), 400), proxyBase: xlm(100)}
		}
		return surfaceTick{proxyPrice: flat(off), ownPrice: flat(off), proxyBase: xlm(100), ownBase: xlm(100)}
	})
	lr := readSurface(t, 24*time.Hour, trades, proxied)
	if lr.quoteSurfaceRefusal(surfaceTol) == "" {
		t.Errorf("gap %.1f bps (aligned=%v overlap=%d) not refused, want the +100 bps whole-window gap to refuse",
			gapBps(lr), lr.surfaceAligned, lr.surfaceOverlap)
	}
}

// Dust own-quote prints at the de-pegged price make late slots "shared" without speaking for the proxy volume there.
func TestQuoteSurface_DustOwnQuoteCannotCoverADepeg(t *testing.T) {
	trades, proxied := buildSurface(t, 24*time.Hour, 10*time.Minute, func(off time.Duration) surfaceTick {
		if off >= 6*time.Hour {
			p := basis(flat(off), 400)
			return surfaceTick{proxyPrice: p, ownPrice: p, proxyBase: xlm(100), ownBase: xlm(1)}
		}
		return surfaceTick{proxyPrice: flat(off), ownPrice: flat(off), proxyBase: xlm(100), ownBase: xlm(100)}
	})
	lr := readSurface(t, 24*time.Hour, trades, proxied)
	if lr.quoteSurfaceRefusal(surfaceTol) == "" {
		t.Errorf("gap %.1f bps (aligned=%v overlap=%d) not refused, want the ~+290 bps whole-window gap to refuse",
			gapBps(lr), lr.surfaceAligned, lr.surfaceOverlap)
	}
}

// Dust proxy prints in the newest slots of a healthy leg must not force a refusal on their own.
func TestQuoteSurface_DustProxyCannotForceARefusal(t *testing.T) {
	trades, proxied := buildSurface(t, 24*time.Hour, 10*time.Minute, func(off time.Duration) surfaceTick {
		if off >= 21*time.Hour {
			return surfaceTick{proxyPrice: basis(flat(off), 100), ownPrice: flat(off), proxyBase: xlm(1), ownBase: xlm(100)}
		}
		return surfaceTick{proxyPrice: flat(off), ownPrice: flat(off), proxyBase: xlm(10_000), ownBase: xlm(100)}
	})
	lr := readSurface(t, 24*time.Hour, trades, proxied)
	if why := lr.quoteSurfaceRefusal(surfaceTol); why != "" {
		t.Errorf("refusal %q (gap %.1f bps): 18 XLM of dust out of 1.26M XLM must not refuse a leg at par", why, gapBps(lr))
	}
}

// One heavy older slot must not out-vote a de-peg in the two newest slots.
func TestQuoteSurface_HeavyOlderSlotCannotOutvoteNewestDepeg(t *testing.T) {
	trades, proxied := buildSurface(t, 24*time.Hour, 10*time.Minute, func(off time.Duration) surfaceTick {
		base := xlm(100)
		if off >= 21*time.Hour && off < 22*time.Hour {
			base = xlm(1000)
		}
		proxy := flat(off)
		if off >= 22*time.Hour {
			proxy = basis(proxy, 100)
		}
		return surfaceTick{proxyPrice: proxy, ownPrice: flat(off), proxyBase: base, ownBase: base}
	})
	lr := readSurface(t, 24*time.Hour, trades, proxied)
	if lr.quoteSurfaceRefusal(surfaceTol) == "" {
		t.Errorf("gap %.1f bps not refused, want the two newest +100 bps slots to refuse", gapBps(lr))
	}
}

// On an exact half-weight split the median is ambiguous; the cautious reading is the larger magnitude.
func TestWeightedMedianGap_ExactTieTakesLargerMagnitude(t *testing.T) {
	at := func(h int) time.Time { return surfaceEnd.Add(time.Duration(h) * time.Hour) }
	cases := []struct {
		gaps []*big.Rat
		want *big.Rat
	}{
		{[]*big.Rat{big.NewRat(0, 1), big.NewRat(1, 100)}, big.NewRat(1, 100)},
		{[]*big.Rat{big.NewRat(-1, 100), big.NewRat(1, 1000)}, big.NewRat(-1, 100)},
	}
	for _, tc := range cases {
		var pts []surfacePoint
		for i, g := range tc.gaps {
			pts = append(pts, surfacePoint{start: at(i), gap: g, weight: big.NewInt(5)})
		}
		if got := weightedMedianGap(pts); got.Cmp(tc.want) != 0 {
			t.Errorf("median of %v = %s, want %s", tc.gaps, got.RatString(), tc.want.RatString())
		}
	}
}

// genSurfaceCase draws one 24 h case: a trend, a proxy basis from a random slot, quiet own-quote tails, dust prints on either side and
// own-quote prints that sometimes follow the de-pegged price.
func genSurfaceCase(rng *rand.Rand) func(off time.Duration) surfaceTick {
	slope := []int64{-6, 0, 6}[rng.IntN(3)]
	depegFrom, depegBps := rng.IntN(24), []int64{0, 150, 400}[rng.IntN(3)]
	ownQuietFrom := 24 - rng.IntN(10)
	vols := []int64{0, 1, 1, 100, 1000}
	proxyVol, ownVol := make([]int64, 24), make([]int64, 24)
	ownFollows := make([]bool, 24)
	for s := range 24 {
		proxyVol[s], ownVol[s] = vols[rng.IntN(len(vols))], vols[rng.IntN(len(vols))]
		ownFollows[s] = rng.IntN(4) == 0
	}
	return func(off time.Duration) surfaceTick {
		s := int(off / time.Hour)
		price := new(big.Rat).Mul(big.NewRat(12, 100), big.NewRat(2400+slope*int64(s), 2400))
		tk := surfaceTick{proxyPrice: price, ownPrice: price}
		if s >= depegFrom {
			tk.proxyPrice = basis(price, depegBps)
			if ownFollows[s] {
				tk.ownPrice = tk.proxyPrice
			}
		}
		if proxyVol[s] > 0 {
			tk.proxyBase = xlm(proxyVol[s])
		}
		if ownVol[s] > 0 && s < ownQuietFrom {
			tk.ownBase = xlm(ownVol[s])
		}
		return tk
	}
}

// slotSums returns per-hour base and quote sums for each surface.
func slotSums(trades []canonical.Trade, proxied map[string]struct{}) (pb, pq, ob, oq map[int64]*big.Int) {
	pb, pq, ob, oq = map[int64]*big.Int{}, map[int64]*big.Int{}, map[int64]*big.Int{}, map[int64]*big.Int{}
	for _, tr := range trades {
		k := tr.Timestamp.Truncate(time.Hour).Unix()
		b, q := ob, oq
		if _, ok := proxied[tr.ID()]; ok {
			b, q = pb, pq
		}
		if b[k] == nil {
			b[k], q[k] = new(big.Int), new(big.Int)
		}
		b[k].Add(b[k], tr.BaseAmount.BigInt())
		q[k].Add(q[k], tr.QuoteAmount.BigInt())
	}
	return pb, pq, ob, oq
}

// oldGapAndUncovered is the whole-window gap (proxy - own) / own and whether more than 5 % of proxy volume lies outside shared slots
// whose weight min(proxy, own) clears 1/48 of the total shared weight — computed here independently of the code under test.
func oldGapAndUncovered(trades []canonical.Trade, proxied map[string]struct{}) (*big.Rat, bool) {
	pb, pq, ob, oq := slotSums(trades, proxied)
	sum := func(m map[int64]*big.Int) *big.Int {
		s := new(big.Int)
		for _, v := range m {
			s.Add(s, v)
		}
		return s
	}
	P, O := sum(pb), sum(ob)
	if P.Sign() == 0 || O.Sign() == 0 {
		return nil, false
	}
	proxyVWAP, ownVWAP := new(big.Rat).SetFrac(sum(pq), P), new(big.Rat).SetFrac(sum(oq), O)
	gap := new(big.Rat).Sub(proxyVWAP, ownVWAP)
	gap.Quo(gap, ownVWAP)
	weights, W := map[int64]*big.Int{}, new(big.Int)
	for k, p := range pb {
		if o, ok := ob[k]; ok {
			w := p
			if o.Cmp(w) < 0 {
				w = o
			}
			weights[k] = w
			W.Add(W, w)
		}
	}
	covered := new(big.Int)
	for k, w := range weights {
		if new(big.Int).Mul(w, big.NewInt(48)).Cmp(W) >= 0 {
			covered.Add(covered, pb[k])
		}
	}
	uncovered := new(big.Int).Sub(P, covered)
	return gap, new(big.Int).Mul(uncovered, big.NewInt(20)).Cmp(P) > 0
}

// The aligned reading may only replace the whole-window reading when it speaks for the served prints: wherever the whole-window gap
// refuses and more than 5 % of proxy volume is outside shared, above-floor slots, the leg is refused.
func TestQuoteSurface_NeverWeakerThanWholeWindow(t *testing.T) {
	tol := big.NewRat(surfaceTol, 10_000)
	hits := 0
	for seed := uint64(1); seed <= 400; seed++ {
		trades, proxied := buildSurface(t, 24*time.Hour, 10*time.Minute, genSurfaceCase(rand.New(rand.NewPCG(seed, 7))))
		old, uncovered := oldGapAndUncovered(trades, proxied)
		if old == nil || !uncovered || new(big.Rat).Abs(old).Cmp(tol) <= 0 {
			continue
		}
		hits++
		if lr := readSurface(t, 24*time.Hour, trades, proxied); lr.quoteSurfaceRefusal(surfaceTol) == "" {
			t.Errorf("seed %d: whole-window gap %.1f bps refuses but the new reading %.1f bps (aligned=%v overlap=%d) passes",
				seed, ratBps(old), gapBps(lr), lr.surfaceAligned, lr.surfaceOverlap)
		}
	}
	if hits < 50 {
		t.Fatalf("only %d of 400 generated cases exercise the guard; the generator no longer covers it", hits)
	}
}
