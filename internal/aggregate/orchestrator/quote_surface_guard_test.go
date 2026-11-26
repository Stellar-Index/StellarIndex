package orchestrator

import (
	"fmt"
	"math/big"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

const surfaceTol = 75

// surfaceOracle is the quote-surface reading recomputed here from raw sums, independently of the code under test: whole is the
// whole-window gap; gaps every proxy-bearing slot's gap (eligible: vs its own-quote VWAP; else vs the whole-window own VWAP); aligned their proxy-weighted mean (nil with no eligible slot); coverage the eligible share of
// proxy volume; lo/hi/rng the envelope of every slot's proxy and own VWAP and its relative range R̂; decides whether coverage is
// >= 95 % and the guard band holds; verdict the gap the leg is judged by.
type surfaceOracle struct {
	whole, aligned, coverage, verdict *big.Rat
	lo, hi, rng                       *big.Rat
	gaps                              []*big.Rat
	eligible                          int
	decides                           bool
}

type sums struct{ base, quote *big.Int }

func (s *sums) add(tr canonical.Trade) {
	if s.base == nil {
		s.base, s.quote = new(big.Int), new(big.Int)
	}
	s.base.Add(s.base, tr.BaseAmount.BigInt())
	s.quote.Add(s.quote, tr.QuoteAmount.BigInt())
}

func (s sums) price() *big.Rat { return new(big.Rat).SetFrac(s.quote, s.base) }

func relGap(proxy, own *big.Rat) *big.Rat {
	g := new(big.Rat).Sub(proxy, own)
	return g.Quo(g, own)
}

type oracleSlot struct {
	proxy, own sums
	venues     map[string]*sums
}

// perHour is n whole units per hour of slot at the 8-decimal scale.
func perHour(n int64, slot time.Duration) *big.Rat {
	return new(big.Rat).SetFrac64(n*100_000_000*int64(slot), int64(time.Hour))
}

func under(v *big.Int, floor *big.Rat) bool { return new(big.Rat).SetInt(v).Cmp(floor) < 0 }

// reference is the slot's venues with >= 10 % of its own base and >= 10k USD per hour, and their sums (nil base with none).
func (s oracleSlot) reference(slot time.Duration) (map[string]*sums, sums) {
	qual, ref := map[string]*sums{}, sums{}
	for name, v := range s.venues {
		if new(big.Int).Mul(v.base, big.NewInt(10)).Cmp(s.own.base) >= 0 && !under(v.quote, perHour(10_000, slot)) {
			qual[name] = v
			ref.base = new(big.Int).Add(orZero(ref.base), v.base)
			ref.quote = new(big.Int).Add(orZero(ref.quote), v.quote)
		}
	}
	return qual, ref
}

func orZero(v *big.Int) *big.Int {
	if v == nil {
		return new(big.Int)
	}
	return v
}

// eligible: >= 2 reference venues, reference base >= proxy base / 4, both surfaces >= 150k XLM and >= 25k USD per hour of slot
// (8-decimal scale), reference venue VWAPs within surfaceTol of the lowest.
func (s oracleSlot) eligible(slot time.Duration) bool {
	qual, ref := s.reference(slot)
	if len(qual) < 2 || new(big.Int).Mul(ref.base, big.NewInt(3)).Cmp(s.proxy.base) < 0 ||
		under(ref.base, perHour(150_000, slot)) || under(s.proxy.base, perHour(150_000, slot)) ||
		under(ref.quote, perHour(25_000, slot)) || under(s.proxy.quote, perHour(25_000, slot)) {
		return false
	}
	var lo, hi *big.Rat
	for _, v := range qual {
		p := v.price()
		if lo == nil || p.Cmp(lo) < 0 {
			lo = p
		}
		if hi == nil || p.Cmp(hi) > 0 {
			hi = p
		}
	}
	return relGap(hi, lo).Cmp(big.NewRat(surfaceTol, 10_000)) <= 0
}

// surfaceOracleOf is the oracle for a 24 h window (hourly slots).
func surfaceOracleOf(trades []canonical.Trade, proxied map[string]struct{}) surfaceOracle {
	return surfaceOracleFor(trades, proxied, 24*time.Hour, time.Hour)
}

func surfaceOracleFor(trades []canonical.Trade, proxied map[string]struct{}, window, slot time.Duration) surfaceOracle {
	slots := map[int64]*oracleSlot{}
	var proxy, own sums
	for _, tr := range trades {
		k := tr.Timestamp.Truncate(slot).UnixNano()
		if slots[k] == nil {
			slots[k] = &oracleSlot{venues: map[string]*sums{}}
		}
		s := slots[k]
		if _, ok := proxied[tr.ID()]; ok {
			proxy.add(tr)
			s.proxy.add(tr)
			continue
		}
		own.add(tr)
		s.own.add(tr)
		if s.venues[tr.Source] == nil {
			s.venues[tr.Source] = &sums{}
		}
		s.venues[tr.Source].add(tr)
	}
	if proxy.base == nil || own.base == nil {
		return surfaceOracle{}
	}
	out := surfaceOracle{whole: relGap(proxy.price(), own.price())}
	weighted, covered := new(big.Rat), new(big.Int)
	for _, s := range slots {
		for _, side := range []sums{s.proxy, s.own} {
			if side.base != nil {
				p := side.price()
				if out.lo == nil || p.Cmp(out.lo) < 0 {
					out.lo = p
				}
				if out.hi == nil || p.Cmp(out.hi) > 0 {
					out.hi = p
				}
			}
		}
		if s.proxy.base == nil {
			continue
		}
		g := relGap(s.proxy.price(), own.price())
		switch {
		case s.own.base != nil && s.eligible(slot):
			_, ref := s.reference(slot)
			g = relGap(s.proxy.price(), ref.price())
			covered.Add(covered, s.proxy.base)
			out.eligible++
		}
		out.gaps = append(out.gaps, g)
		weighted.Add(weighted, new(big.Rat).Mul(g, new(big.Rat).SetInt(s.proxy.base)))
	}
	out.coverage = new(big.Rat).SetFrac(covered, proxy.base)
	out.rng = new(big.Rat).Sub(new(big.Rat).Quo(out.hi, out.lo), big.NewRat(1, 1))
	out.verdict = out.whole
	if out.eligible > 0 {
		out.aligned = weighted.Quo(weighted, new(big.Rat).SetInt(proxy.base))
		// Guard band: |r| + (1-c)·R̂·(1+|r|) <= tolerance.
		abs := new(big.Rat).Abs(out.aligned)
		band := new(big.Rat).Sub(big.NewRat(1, 1), out.coverage)
		band.Mul(band, out.rng).Mul(band, new(big.Rat).Add(big.NewRat(1, 1), abs)).Add(band, abs)
		windowFloor := perHour(50_000, window)
		out.decides = out.coverage.Cmp(big.NewRat(19, 20)) >= 0 && band.Cmp(big.NewRat(surfaceTol, 10_000)) <= 0 &&
			!under(proxy.quote, windowFloor) && !under(own.quote, windowFloor)
		out.verdict = out.aligned
		if !out.decides && new(big.Rat).Abs(out.whole).Cmp(new(big.Rat).Abs(out.aligned)) > 0 {
			out.verdict = out.whole
		}
	}
	return out
}

func exceeds(r *big.Rat) bool {
	return r != nil && new(big.Rat).Abs(r).Cmp(big.NewRat(surfaceTol, 10_000)) > 0
}

// A de-peg in the hours the own-quote venues are silent is served through the whole-window VWAP, so the aligned reading (which only
// sees eligible slots) must not replace it.
func TestQuoteSurface_QuietTailDepegIsRefused(t *testing.T) {
	trades, proxied := buildSurface(t, 24*time.Hour, 10*time.Minute, func(off time.Duration) surfaceTick {
		if off >= 18*time.Hour {
			return surfaceTick{proxyPrice: basis(flat(off), 400), proxyBase: xlm(100)}
		}
		return surfaceTick{proxyPrice: flat(off), ownPrice: flat(off), proxyBase: xlm(100), ownBase: xlm(100)}
	})
	lr := readSurface(t, 24*time.Hour, trades, proxied)
	if lr.quoteSurfaceRefusal(surfaceTol) == "" || lr.surfaceAligned {
		t.Errorf("gap %.1f bps (aligned=%v overlap=%d) not refused, want the +100 bps whole-window gap to refuse",
			gapBps(lr), lr.surfaceAligned, lr.surfaceOverlap)
	}
}

// Dust proxy prints in the newest slots of a healthy leg must not force a refusal on their own.
func TestQuoteSurface_DustProxyCannotForceARefusal(t *testing.T) {
	trades, proxied := buildSurface(t, 24*time.Hour, 10*time.Minute, func(off time.Duration) surfaceTick {
		if off >= 21*time.Hour {
			return surfaceTick{proxyPrice: basis(flat(off), 100), ownPrice: flat(off), proxyBase: xlm(1), ownBase: xlm(100)}
		}
		return surfaceTick{proxyPrice: flat(off), ownPrice: flat(off), proxyBase: xlm(10_000), ownBase: xlm(5_000)}
	})
	lr := readSurface(t, 24*time.Hour, trades, proxied)
	if why := lr.quoteSurfaceRefusal(surfaceTol); why != "" {
		t.Errorf("refusal %q (gap %.1f bps): 18k XLM of dust out of 1.26B XLM must not refuse a leg at par", why, gapBps(lr))
	}
}

// depegged2pc is the T4 leg: flat price, stablecoin prints 2 % below real USD all window, the 4:1 shifted volume profile.
func depegged2pc(off time.Duration) surfaceTick {
	pb, ob := shiftedVolume(off)
	return surfaceTick{proxyPrice: basis(flat(off), -200), ownPrice: flat(off), proxyBase: pb, ownBase: ob}
}

// firstPass returns the smallest k in [0, limit] (by step) for which passes(k), or -1.
func firstPass(step, limit int64, passes func(k int64) bool) int64 {
	for k := int64(0); k <= limit; k += step {
		if passes(k) {
			return k
		}
	}
	return -1
}

// T4(a): an attacker venue prints at the de-pegged price, k % of the honest own-quote volume, spread in proportion to the proxy volume
// (its best allocation against a proxy-weighted mean). Masking needs own VWAP within 75 bps of the proxy: k >= ~170 % on the whole
// window (a = 0.0125/0.01985 of own volume). Option A must cost no less: its prints disagree with the honest venues by 200 bps, so its
// slots fall out and the whole window judges. Pricing within the agreement bound instead moves a slot's reference by <= 74 bps, which
// leaves >= 126 bps of a 200 bps de-peg showing at any volume.
func TestQuoteSurface_ExtraVenueCannotMaskADepegCheaperThanWholeWindow(t *testing.T) {
	window := 24 * time.Hour
	honestOwn, proxyTotal := int64(6*(16*250+8*1000)), int64(6*(16*1000+8*250))
	build := func(k int64, attackerPrice func(time.Duration) *big.Rat) ([]canonical.Trade, map[string]struct{}) {
		return buildSurface(t, window, 10*time.Minute, func(off time.Duration) surfaceTick {
			tk := depegged2pc(off)
			if k > 0 {
				tk.extraBase = new(big.Int).Mul(tk.proxyBase, big.NewInt(k*honestOwn))
				tk.extraBase.Quo(tk.extraBase, big.NewInt(100*proxyTotal))
				tk.extraPrice = attackerPrice(off)
			}
			return tk
		})
	}
	atProxy := func(off time.Duration) *big.Rat { return basis(flat(off), -200) }
	xOld := firstPass(5, 400, func(k int64) bool { return !exceeds(surfaceOracleOf(build(k, atProxy)).whole) })
	xNew := firstPass(5, 400, func(k int64) bool {
		trades, proxied := build(k, atProxy)
		return readSurface(t, window, trades, proxied).quoteSurfaceRefusal(surfaceTol) == ""
	})
	t.Logf("attack cost to mask a 2%% de-peg, %% of honest own-quote volume: whole window %d, option A %d", xOld, xNew)
	if xOld < 150 || (xNew != -1 && xNew < xOld) {
		t.Errorf("attack cost: whole window %d %%, option A %d %%; want option A no cheaper than the whole window (~170 %%)", xOld, xNew)
	}
	agreeing := func(off time.Duration) *big.Rat { return new(big.Rat).Mul(flat(off), big.NewRat(10_000, 10_074)) }
	for _, k := range []int64{100, 1000, 10_000} {
		trades, proxied := build(k, agreeing)
		if lr := readSurface(t, window, trades, proxied); lr.quoteSurfaceRefusal(surfaceTol) == "" || gapBps(lr) > -125 {
			t.Errorf("agreeing attacker at %d %%: gap %.1f bps, want refused at <= -125 bps", k, gapBps(lr))
		}
	}
}

// T4(b): eligibility cannot be removed from de-pegged slots to cherry-pick the clean ones: the lost coverage hands the verdict back to
// the larger of the aligned and whole-window gaps.
func TestQuoteSurface_IneligibleDepeggedSlotsStillRefuse(t *testing.T) {
	window := 24 * time.Hour
	cases := map[string]func(off time.Duration, tk *surfaceTick){
		"one own venue in the de-pegged slots": func(_ time.Duration, tk *surfaceTick) { tk.soleOwn = true },
		"a disagreeing qualifying venue in the de-pegged slots": func(off time.Duration, tk *surfaceTick) {
			tk.extraPrice, tk.extraBase = basis(flat(off), 500), xlm(20) // 1/6 of own base, 14.4k USD per hour
		},
	}
	for name, spoil := range cases {
		trades, proxied := buildSurface(t, window, 10*time.Minute, func(off time.Duration) surfaceTick {
			tk := surfaceTick{proxyPrice: flat(off), ownPrice: flat(off), proxyBase: xlm(100), ownBase: xlm(100)}
			if off >= 12*time.Hour {
				tk.proxyPrice = basis(flat(off), -200)
				spoil(off, &tk)
			}
			return tk
		})
		lr := readSurface(t, window, trades, proxied)
		if lr.quoteSurfaceRefusal(surfaceTol) == "" || lr.surfaceAligned || lr.surfaceOverlap != 12 {
			t.Errorf("%s: gap %.1f bps aligned=%v overlap=%d, want refused on the whole window with 12 eligible slots",
				name, gapBps(lr), lr.surfaceAligned, lr.surfaceOverlap)
		}
	}
}

// T4(c): an attacker trading on the honest venues themselves passes the agreement rule, so it concentrates in slots where honest
// own volume is thin against heavy proxy volume (16 h of 1000 proxy : 50 own, 8 h of 50 : 1000; 2 % de-peg). Those slots are
// ineligible until own volume reaches 1/3 of proxy, so the attack must bring ~29 % of the window's proxy volume (~54 % of honest own
// volume); judged against thin slots it would need ~9 %. The whole-window compare needs ~170 % of honest own volume.
func TestQuoteSurface_ThinSlotAttackCostScalesWithProxyVolume(t *testing.T) {
	window := 24 * time.Hour
	xlmUSD := mkPair(t, "crypto", "XLM", "fiat", "USD")
	build := func(k int64) ([]canonical.Trade, map[string]struct{}) {
		trades, proxied := buildSurface(t, window, 10*time.Minute, func(off time.Duration) surfaceTick {
			pb, ob := xlm(1000), xlm(50)
			if off >= 16*time.Hour {
				pb, ob = xlm(50), xlm(1000)
			}
			return surfaceTick{proxyPrice: basis(flat(off), -200), ownPrice: flat(off), proxyBase: pb, ownBase: ob}
		})
		for _, tr := range append([]canonical.Trade(nil), trades...) {
			if _, ok := proxied[tr.ID()]; !ok || k == 0 {
				continue
			}
			base := new(big.Int).Mul(tr.BaseAmount.BigInt(), big.NewInt(k))
			base.Quo(base, big.NewInt(200))
			for _, venue := range ownVenues {
				trades = append(trades, surfacePrint(xlmUSD, venue, tr.Timestamp.Add(time.Second), base, basis(flat(0), -200)))
			}
		}
		return trades, proxied
	}
	xNew := firstPass(1, 60, func(k int64) bool {
		trades, proxied := build(k)
		return readSurface(t, window, trades, proxied).quoteSurfaceRefusal(surfaceTol) == ""
	})
	xOld := firstPass(5, 200, func(k int64) bool { return !exceeds(surfaceOracleOf(build(k)).whole) })
	const proxyTotal, honestOwn = 6 * (16*1000 + 8*50), 6 * (16*50 + 8*1000)
	t.Logf("attack cost, %% of proxy volume (%% of honest own): option A %d (%d), whole window %d (%d)",
		xNew, xNew*proxyTotal/honestOwn, xOld, xOld*proxyTotal/honestOwn)
	if xNew != -1 && xNew < 25 {
		t.Errorf("a %d %%-of-proxy own-venue attack masked a 2 %% de-peg; want >= 25 %% (thin slots must not vouch)", xNew)
	}
}

// surfaceCase is one generated window: at prints it, depegBps is the true basis (bps) of the proxy print at ts, slopePct the window's
// total price move in percent.
type surfaceCase struct {
	at       func(off time.Duration) surfaceTick
	depegBps func(ts time.Time) int64
	slopePct int64
}

// genSurfaceCase draws one case for a window of slot-length slots starting at start: a trend, a basis from a random slot, random
// per-slot volume profiles and, in thin-evidence cases, quiet own-quote tails, one-venue slots and a third venue printing at the
// stablecoin price. Prices change only at slot boundaries (absolute-time aligned, like the code's slots).
func genSurfaceCase(rng *rand.Rand, start time.Time, window, slot time.Duration) surfaceCase {
	m := int(window / slot)
	n := m + 2
	first := start.Truncate(slot)
	idx := func(ts time.Time) int { return int(ts.Truncate(slot).Sub(first) / slot) }
	slope := []int64{-6, 0, 6}[rng.IntN(3)]
	depegFrom, depegBps := []int{0, rng.IntN(n), m - 1}[rng.IntN(3)], []int64{0, -150, 150, 400, 5000}[rng.IntN(5)]
	thin, shifted, spoilDepeg := rng.IntN(2) == 0, rng.IntN(2) == 0, rng.IntN(2) == 0
	ownQuietFrom, proxyVols, ownVols := n, []int64{334, 1000}, []int64{334, 1000}
	if thin {
		ownQuietFrom, proxyVols, ownVols = n-rng.IntN(n/2+1), []int64{0, 334, 1000}, []int64{0, 50, 334, 1000}
	}
	proxyVol, ownVol := make([]int64, n), make([]int64, n)
	sole, extra := make([]bool, n), make([]bool, n)
	for s := range n {
		proxyVol[s], ownVol[s] = proxyVols[rng.IntN(len(proxyVols))], ownVols[rng.IntN(len(ownVols))]
		if shifted && !thin {
			proxyVol[s], ownVol[s] = 1000, 334
			if 3*s >= 2*n {
				proxyVol[s], ownVol[s] = 334, 1000
			}
		}
		sole[s], extra[s] = thin && rng.IntN(4) == 0, thin && rng.IntN(4) == 0
	}
	bps := func(ts time.Time) int64 {
		if idx(ts) >= depegFrom {
			return depegBps
		}
		return 0
	}
	at := func(off time.Duration) surfaceTick {
		ts := start.Add(off)
		s := idx(ts)
		price := new(big.Rat).Mul(big.NewRat(12, 100), big.NewRat(int64(100*n)+slope*int64(s), int64(100*n)))
		tk := surfaceTick{proxyPrice: basis(price, bps(ts)), ownPrice: price, soleOwn: sole[s]}
		if proxyVol[s] > 0 {
			tk.proxyBase = xlm(proxyVol[s])
		}
		if ownVol[s] > 0 && s < ownQuietFrom {
			tk.ownBase = xlm(ownVol[s])
		}
		if extra[s] {
			tk.extraPrice, tk.extraBase = tk.proxyPrice, xlm(ownVol[s]/5+1)
		}
		if spoilDepeg && bps(ts) != 0 { // a venue over the floor disagreeing, so de-pegged slots are ineligible
			tk.extraPrice, tk.extraBase = basis(price, 100), xlm(ownVol[s]/5+1)
		}
		return tk
	}
	return surfaceCase{at: at, depegBps: bps, slopePct: slope}
}

// trueDepeg is the proxy-base-weighted mean of the generated basis over every proxy print.
func trueDepeg(trades []canonical.Trade, proxied map[string]struct{}, depegBps func(time.Time) int64) *big.Rat {
	sum, base := new(big.Rat), new(big.Int)
	for _, tr := range trades {
		if _, ok := proxied[tr.ID()]; ok {
			b := tr.BaseAmount.BigInt()
			sum.Add(sum, new(big.Rat).SetFrac(new(big.Int).Mul(b, big.NewInt(depegBps(tr.Timestamp))), big.NewInt(10_000)))
			base.Add(base, b)
		}
	}
	return sum.Quo(sum, new(big.Rat).SetInt(base))
}

type surfaceStats struct{ alone, fallback, relieved, allOver int }

// checkGenerated judges one generated window against the oracle and the decide-alone bound, counting what it exercised.
func checkGenerated(t *testing.T, name string, window, slot time.Duration, c surfaceCase, trades []canonical.Trade,
	proxied map[string]struct{}, st *surfaceStats,
) {
	t.Helper()
	want := surfaceOracleFor(trades, proxied, window, slot)
	if want.whole == nil {
		return
	}
	lr := readSurface(t, window, trades, proxied)
	refused := lr.quoteSurfaceRefusal(surfaceTol) != ""
	if lr.surfaceAligned && exceeds(want.whole) && !refused {
		st.relieved++
		// |reading - D| <= (1 - c) * R * (1 + |d|): a pass may hide at most that much true de-peg beyond the tolerance.
		slack := new(big.Rat).Sub(big.NewRat(1, 1), lr.surfaceCoverage)
		slack.Mul(slack, big.NewRat(abs64(c.slopePct), 100)).Mul(slack, big.NewRat(10_400, 10_000))
		limit := slack.Add(slack, big.NewRat(surfaceTol, 10_000))
		if d := trueDepeg(trades, proxied, c.depegBps); new(big.Rat).Abs(d).Cmp(limit) > 0 {
			t.Errorf("%s: trend relief passed %.1f bps with a true de-peg of %.1f bps, over the %.1f bps bound", name,
				gapBps(lr), ratBps(d), ratBps(limit))
		}
	}
	if lr.surfaceAligned != want.decides || lr.surfaceOverlap != want.eligible || lr.surfaceGap.Cmp(want.verdict) != 0 {
		t.Errorf("%s: aligned=%v overlap=%d gap %.2f bps; oracle decides=%v eligible=%d verdict %.2f bps (coverage %s)", name,
			lr.surfaceAligned, lr.surfaceOverlap, gapBps(lr), want.decides, want.eligible, ratBps(want.verdict),
			want.coverage.FloatString(3))
		return
	}
	if want.decides {
		st.alone++
	} else {
		st.fallback++
		if exceeds(want.whole) && !refused {
			t.Errorf("%s: coverage %s, whole-window gap %.1f bps refuses but the leg passed at %.1f bps", name,
				want.coverage.FloatString(3), ratBps(want.whole), gapBps(lr))
		}
	}
	if want.eligible > 0 && everyGapOverSameSign(want.gaps) {
		st.allOver++
		if !refused {
			t.Errorf("%s: every proxy slot over 75 bps with one sign, leg passed at %.1f bps", name, gapBps(lr))
		}
	}
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// T5: over generated windows of each size, hour- and minute-aligned (partial edge slots), against the independent oracle: the
// reading equals the oracle's verdict exactly in both coverage regimes; below 95 % coverage a whole-window gap over 75 bps always
// refuses; every proxy slot over 75 bps with one sign refuses; and a decide-alone pass that the whole window would have refused hides
// no more true de-peg than the stated bound allows. The exact-verdict check restates the rule, so it catches drift from the written
// rule, not a wrong rule; the true-price checks here and in ContinuousTrendsNeverAcceptAHiddenDepeg are the authority.
func TestQuoteSurface_GeneratedWindowsHoldTheDecisionRule(t *testing.T) {
	sizes := []struct{ window, slot, step time.Duration }{
		{24 * time.Hour, time.Hour, 10 * time.Minute},
		{time.Hour, 2 * time.Minute, 20 * time.Second},
		{5 * time.Minute, time.Minute, 10 * time.Second},
	}
	var total surfaceStats
	for _, sz := range sizes {
		var st surfaceStats
		for _, end := range []time.Time{surfaceEnd, surfaceOddEnd} {
			for seed := uint64(1); seed <= 200; seed++ {
				c := genSurfaceCase(rand.New(rand.NewPCG(seed, 7)), end.Add(-sz.window), sz.window, sz.slot)
				trades, proxied := buildSurfaceEnding(t, end, sz.window, sz.step, c.at)
				name := fmt.Sprintf("%v ending %s seed %d", sz.window, end.Format("15:04"), seed)
				checkGenerated(t, name, sz.window, sz.slot, c, trades, proxied, &st)
			}
		}
		t.Logf("%v: decided alone %d, fallback %d, trend relieved %d, all slots over %d", sz.window, st.alone, st.fallback,
			st.relieved, st.allOver)
		if st.alone < 50 || st.fallback < 50 {
			t.Errorf("%v generator coverage: decided alone %d, fallback %d; want >= 50 each", sz.window, st.alone, st.fallback)
		}
		total.relieved += st.relieved
		total.allOver += st.allOver
	}
	if total.relieved < 10 || total.allOver < 100 {
		t.Fatalf("generator coverage: trend relieved %d, all slots over %d; want >= 10, 100", total.relieved, total.allOver)
	}
}

func everyGapOverSameSign(gaps []*big.Rat) bool {
	if len(gaps) == 0 {
		return false
	}
	sign := gaps[0].Sign()
	for _, g := range gaps {
		if !exceeds(g) || g.Sign() != sign {
			return false
		}
	}
	return true
}

// lastHourDepeg is a 24 h leg at par (1000/1000 per 10-minute tick) whose stablecoin prints sit bps off in the window's last hour,
// with spoil applied to those ticks.
func lastHourDepeg(t *testing.T, end time.Time, bps int64, spoil func(off time.Duration, tk *surfaceTick)) legRef {
	t.Helper()
	trades, proxied := buildSurfaceEnding(t, end, 24*time.Hour, 10*time.Minute, func(off time.Duration) surfaceTick {
		tk := surfaceTick{proxyPrice: flat(off), ownPrice: flat(off), proxyBase: xlm(1000), ownBase: xlm(1000)}
		if off >= 23*time.Hour {
			tk.proxyPrice = basis(flat(off), bps)
			spoil(off, &tk)
		}
		return tk
	})
	return readSurface(t, 24*time.Hour, trades, proxied)
}

// A de-peg confined to slots that are ineligible (< 5 % of proxy volume, so the aligned reading decides alone) still counts in full:
// the whole-window compare reads ~+104 bps (+2500) and ~+208 bps (+5000), and so must the aligned reading. Ineligibility comes from
// a dust print on a third venue, from two honest venues 124 bps apart in a fast move's onset hour, or from silent USD venues plus
// one print at the de-pegged price (which must not set that slot's reference); also with a minute-aligned end, where
// the de-peg spans a partial slot.
func TestQuoteSurface_DepegInIneligibleSlotsCountsInFull(t *testing.T) {
	spoils := map[string]func(off time.Duration, tk *surfaceTick){
		"dust on a third venue": func(off time.Duration, tk *surfaceTick) {
			tk.extraPrice, tk.extraBase = basis(flat(off), 100), xlm(1)
		},
		"venues 124 bps apart": func(off time.Duration, tk *surfaceTick) {
			tk.soleOwn, tk.extraPrice, tk.extraBase = true, basis(flat(off), 124), xlm(1000)
		},
		"silent USD, one print at the de-pegged price": func(off time.Duration, tk *surfaceTick) {
			tk.ownBase = nil
			if off == 23*time.Hour {
				tk.extraPrice, tk.extraBase = tk.proxyPrice, xlm(1)
			}
		},
	}
	for name, spoil := range spoils {
		for _, end := range []time.Time{surfaceEnd, surfaceOddEnd} {
			for _, bps := range []int64{2500, 5000} {
				lr := lastHourDepeg(t, end, bps, spoil)
				if lr.quoteSurfaceRefusal(surfaceTol) == "" || gapBps(lr) < float64(bps)/24-5 {
					t.Errorf("%s, end %s, +%d bps in the last hour: gap %.1f bps aligned=%v overlap=%d, want refused at >= ~%.0f bps",
						name, end.Format("15:04"), bps, gapBps(lr), lr.surfaceAligned, lr.surfaceOverlap, float64(bps)/24)
				}
			}
		}
	}
}

// An ineligible slot's own prints never set its reference: with a +100 bps de-peg everywhere and the last hour's USD venues silent,
// one print at 1.312x the price there would read that slot at -2300 bps, larger in magnitude than its +100 bps against the
// whole-window VWAP, and cancel the other 23 hours (23/24 x 100 - 1/24 x 2300 ~ 0).
func TestQuoteSurface_IneligibleSlotPrintCannotCancelADepeg(t *testing.T) {
	trades, proxied := buildSurface(t, 24*time.Hour, 10*time.Minute, func(off time.Duration) surfaceTick {
		tk := surfaceTick{proxyPrice: basis(flat(off), 100), ownPrice: flat(off), proxyBase: xlm(1000), ownBase: xlm(1000)}
		if off >= 23*time.Hour {
			tk.ownBase = nil
			if off == 23*time.Hour {
				tk.extraPrice, tk.extraBase = new(big.Rat).Mul(flat(off), big.NewRat(1312, 1000)), xlm(1)
			}
		}
		return tk
	})
	lr := readSurface(t, 24*time.Hour, trades, proxied)
	if lr.quoteSurfaceRefusal(surfaceTol) == "" || gapBps(lr) < 99 {
		t.Errorf("gap %.1f bps aligned=%v, want ~+100 bps refused", gapBps(lr), lr.surfaceAligned)
	}
}

// The cost of judging ineligible slots against the whole-window reference: under a pure trend an
// ineligible slot reads its price's distance from the window's USD VWAP. One ineligible end hour (4.2 % of proxy volume, so the
// coverage is 95.8 %) sits ~0.48m/(1+m/2) above the VWAP of a linear move m, reading r ~ 1/24 of that. The guard band lets r decide
// alone while |r| + (1/24)·R̂·(1+|r|) <= 75 bps, R̂ ~ m: up to m ~ 12 % (6 %: r ~ +12, band ~ +37 bps). Beyond it the whole-window gap
// joins; with volume uniform across the day that gap is ~0, so 15 % and 40 % still pass (~+28, ~+67 bps) and 50 % refuses (~+80 bps,
// threshold ~46 %). Under shifted volume the whole-window gap is the old trend artefact, so past the band the old false alarm returns.
func TestQuoteSurface_TrendWithIneligibleSlotsPassesWhileTheirShareIsSmall(t *testing.T) {
	for _, tc := range []struct {
		movePct        int64
		refused, alone bool
	}{{6, false, true}, {15, false, false}, {40, false, false}, {50, true, false}} {
		price := func(off time.Duration) *big.Rat {
			frac := new(big.Rat).SetFrac64(int64(off), int64(24*time.Hour))
			return new(big.Rat).Mul(big.NewRat(12, 100), frac.Mul(frac, big.NewRat(tc.movePct, 100)).Add(frac, big.NewRat(1, 1)))
		}
		trades, proxied := buildSurface(t, 24*time.Hour, 10*time.Minute, func(off time.Duration) surfaceTick {
			return surfaceTick{
				proxyPrice: price(off), ownPrice: price(off), proxyBase: xlm(1000), ownBase: xlm(1000),
				soleOwn: off >= 23*time.Hour,
			}
		})
		lr := readSurface(t, 24*time.Hour, trades, proxied)
		if got := lr.quoteSurfaceRefusal(surfaceTol) != ""; got != tc.refused || lr.surfaceAligned != tc.alone || lr.surfaceOverlap != 23 {
			t.Errorf("%d %% move, last hour ineligible: refused=%v gap %.1f bps aligned=%v overlap=%d, want refused=%v aligned=%v",
				tc.movePct, got, gapBps(lr), lr.surfaceAligned, lr.surfaceOverlap, tc.refused, tc.alone)
		}
	}
}
