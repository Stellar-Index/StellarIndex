package orchestrator

import (
	"fmt"
	"math/big"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// trendCase is one continuous-trend window: price drifts linearly or exponentially by movePct over the window, 1..3 slots at the
// start, end or middle are ineligible (USD-silent, one venue, or a disagreeing dust print), and a de-peg of 0..300 bps sits
// everywhere, only in the ineligible slots or only in the eligible ones.
type trendCase struct {
	at    func(off time.Duration) surfaceTick
	price func(ts time.Time) *big.Rat
	bps   func(ts time.Time) int64
	desc  string
}

func genTrendCase(rng *rand.Rand, start time.Time, window, slot, step time.Duration) trendCase {
	first := start.Truncate(slot)
	idx := func(ts time.Time) int { return int(ts.Truncate(slot).Sub(first) / slot) }
	slots := int(window/slot) + 1
	move := big.NewRat(int64(1+rng.IntN(15))*[]int64{-1, 1}[rng.IntN(2)], 100)
	exponential := rng.IntN(2) == 0
	ticks := int(window / step)
	perTick := new(big.Rat).Add(big.NewRat(1, 1), new(big.Rat).Quo(move, big.NewRat(int64(ticks), 1)))
	prices := make([]*big.Rat, ticks+1)
	prices[0] = big.NewRat(12, 100)
	for i := 1; i <= ticks; i++ {
		if exponential {
			prices[i] = new(big.Rat).Mul(prices[i-1], perTick)
			continue
		}
		frac := new(big.Rat).Mul(move, big.NewRat(int64(i), int64(ticks)))
		prices[i] = new(big.Rat).Mul(big.NewRat(12, 100), frac.Add(frac, big.NewRat(1, 1)))
	}
	price := func(ts time.Time) *big.Rat { return prices[int(ts.Sub(start)/step)] }

	k, where, kind := 1+rng.IntN(3), rng.IntN(3), rng.IntN(3)
	bad := map[int]bool{}
	for j := range k {
		switch where {
		case 0:
			bad[j] = true
		case 1:
			bad[slots-1-j] = true
		default:
			bad[slots/2+j] = true
		}
	}
	badVol := []int64{250, 1000}[rng.IntN(2)]
	depeg, place := int64(rng.IntN(301))*[]int64{-1, 1}[rng.IntN(2)], rng.IntN(3)
	bps := func(ts time.Time) int64 {
		if b := bad[idx(ts)]; place == 0 || (place == 1) == b {
			return depeg
		}
		return 0
	}
	at := func(off time.Duration) surfaceTick {
		ts := start.Add(off)
		p := price(ts)
		tk := surfaceTick{proxyPrice: basis(p, bps(ts)), ownPrice: p, proxyBase: xlm(1000), ownBase: xlm(1000)}
		if bad[idx(ts)] {
			tk.proxyBase = xlm(badVol)
			switch kind {
			case 0:
				tk.ownBase = nil
			case 1:
				tk.soleOwn = true
			default:
				tk.extraPrice, tk.extraBase = basis(p, 200), xlm(1)
			}
		}
		return tk
	}
	desc := fmt.Sprintf("move %s exp=%v, %d bad slots at %d (kind %d, vol %d), de-peg %+d bps placed %d",
		move.FloatString(2), exponential, k, where, kind, badVol, depeg, place)
	return trendCase{at: at, price: price, bps: bps, desc: desc}
}

// trendResidual is the proven slack beyond the tolerance for a decide-alone pass: (1-c)·[R̂·max(0, d - |r|) + (1+d)(1+R̂)·E], d the
// largest slot de-peg, E the largest excursion of a slot's proxy-weighted true price outside the observed envelope [lo, hi].
func trendResidual(trades []canonical.Trade, proxied map[string]struct{}, slot time.Duration, c trendCase, o surfaceOracle,
	r *big.Rat,
) *big.Rat {
	base, wsum := map[int64]*big.Int{}, map[int64]*big.Rat{}
	d := new(big.Rat)
	for _, tr := range trades {
		if _, ok := proxied[tr.ID()]; !ok {
			continue
		}
		k := tr.Timestamp.Truncate(slot).UnixNano()
		if base[k] == nil {
			base[k], wsum[k] = new(big.Int), new(big.Rat)
		}
		b := tr.BaseAmount.BigInt()
		base[k].Add(base[k], b)
		wsum[k].Add(wsum[k], new(big.Rat).Mul(new(big.Rat).SetInt(b), c.price(tr.Timestamp)))
		if bd := big.NewRat(abs64(c.bps(tr.Timestamp)), 10_000); bd.Cmp(d) > 0 {
			d = bd
		}
	}
	e := new(big.Rat)
	for k, b := range base {
		p := new(big.Rat).Quo(wsum[k], new(big.Rat).SetInt(b))
		for _, x := range []*big.Rat{new(big.Rat).Quo(p, o.hi), new(big.Rat).Quo(o.lo, p)} {
			if x.Sub(x, big.NewRat(1, 1)).Cmp(e) > 0 {
				e = x
			}
		}
	}
	one := big.NewRat(1, 1)
	slack := new(big.Rat).Sub(d, new(big.Rat).Abs(r))
	if slack.Sign() < 0 {
		slack = new(big.Rat)
	}
	slack.Mul(slack, o.rng)
	excursion := new(big.Rat).Mul(new(big.Rat).Add(one, d), new(big.Rat).Add(one, o.rng))
	slack.Add(slack, excursion.Mul(excursion, e))
	return slack.Mul(slack, new(big.Rat).Sub(one, o.coverage))
}

// S5: continuous trends with ineligible slots. A decide-alone pass never hides a true proxy-weighted de-peg beyond the tolerance
// plus the proven residual; a fallback pass is never more lenient than the whole-window compare (anything it hides, the old check
// hid too).
func TestQuoteSurface_ContinuousTrendsNeverAcceptAHiddenDepeg(t *testing.T) {
	tol := big.NewRat(surfaceTol, 10_000)
	sizes := []struct{ window, slot, step time.Duration }{
		{24 * time.Hour, time.Hour, 10 * time.Minute},
		{time.Hour, 2 * time.Minute, 20 * time.Second},
	}
	maxResidual, alone, fallback, hidden := new(big.Rat), 0, 0, 0
	for _, sz := range sizes {
		for _, end := range []time.Time{surfaceEnd, surfaceOddEnd} {
			for seed := uint64(1); seed <= 300; seed++ {
				c := genTrendCase(rand.New(rand.NewPCG(seed, 11)), end.Add(-sz.window), sz.window, sz.slot, sz.step)
				trades, proxied := buildSurfaceEnding(t, end, sz.window, sz.step, c.at)
				lr := readSurface(t, sz.window, trades, proxied)
				if lr.quoteSurfaceRefusal(surfaceTol) != "" {
					continue
				}
				d := new(big.Rat).Abs(trueDepeg(trades, proxied, c.bps))
				name := fmt.Sprintf("%v ending %s seed %d (%s)", sz.window, end.Format("15:04"), seed, c.desc)
				if !lr.surfaceAligned {
					fallback++
					if d.Cmp(tol) > 0 {
						hidden++
					}
					if o := surfaceOracleFor(trades, proxied, sz.window, sz.slot); exceeds(o.whole) {
						t.Errorf("%s: fallback passed but the whole-window gap %.1f bps refuses", name, ratBps(o.whole))
					}
					continue
				}
				alone++
				o := surfaceOracleFor(trades, proxied, sz.window, sz.slot)
				res := trendResidual(trades, proxied, sz.slot, c, o, lr.surfaceGap)
				if res.Cmp(maxResidual) > 0 {
					maxResidual = res
				}
				if d.Cmp(new(big.Rat).Add(tol, res)) > 0 {
					t.Errorf("%s: decided alone and passed at %.1f bps with a true de-peg of %.1f bps (bound %.1f + %.2f bps)",
						name, gapBps(lr), ratBps(d), ratBps(tol), ratBps(res))
				}
			}
		}
	}
	t.Logf("passed alone %d, passed on fallback %d (of which %d hide a de-peg > 75 bps, as the whole-window check does), largest residual %.2f bps",
		alone, fallback, hidden, ratBps(maxResidual))
	if alone < 100 {
		t.Fatalf("only %d decide-alone passes; the generator no longer exercises the guard", alone)
	}
}
