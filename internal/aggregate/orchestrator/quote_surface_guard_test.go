package orchestrator

import (
	"math/big"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

const surfaceTol = 75

// surfaceOracle is the quote-surface reading of a 24 h window recomputed here from raw sums (hourly slots), independently of the code
// under test: whole is the whole-window gap; aligned the proxy-weighted mean gap of eligible slots (nil if none), gaps their gaps;
// coverage their share of proxy volume; decides whether that share is >= 95 %.
type surfaceOracle struct {
	whole, aligned, coverage *big.Rat
	gaps                     []*big.Rat
	decides                  bool
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

// hourFloor is 10k USD at the test venues' 8-decimal scale.
var hourFloor = new(big.Int).Mul(big.NewInt(10_000), big.NewInt(100_000_000))

// eligible: >= 2 own venues, own base >= proxy base / 4, each surface >= 10k USD, venue VWAPs within surfaceTol of the lowest.
func (s oracleSlot) eligible() bool {
	if len(s.venues) < 2 || new(big.Int).Mul(s.own.base, big.NewInt(4)).Cmp(s.proxy.base) < 0 ||
		s.own.quote.Cmp(hourFloor) < 0 || s.proxy.quote.Cmp(hourFloor) < 0 {
		return false
	}
	var lo, hi *big.Rat
	for _, v := range s.venues {
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

func surfaceOracleOf(trades []canonical.Trade, proxied map[string]struct{}) surfaceOracle {
	slots := map[int64]*oracleSlot{}
	var proxy, own sums
	for _, tr := range trades {
		k := tr.Timestamp.Truncate(time.Hour).Unix()
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
		if s.proxy.base == nil || s.own.base == nil || !s.eligible() {
			continue
		}
		g := relGap(s.proxy.price(), s.own.price())
		out.gaps = append(out.gaps, g)
		weighted.Add(weighted, new(big.Rat).Mul(g, new(big.Rat).SetInt(s.proxy.base)))
		covered.Add(covered, s.proxy.base)
	}
	out.coverage = new(big.Rat).SetFrac(covered, proxy.base)
	out.decides = out.coverage.Cmp(big.NewRat(19, 20)) >= 0
	if len(out.gaps) > 0 {
		out.aligned = weighted.Quo(weighted, new(big.Rat).SetInt(covered))
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
		if lr := readSurface(t, window, trades, proxied); lr.quoteSurfaceRefusal(surfaceTol) == "" || !lr.surfaceAligned {
			t.Errorf("agreeing attacker at %d %%: gap %.1f bps aligned=%v, want refused on the aligned reading", k, gapBps(lr), lr.surfaceAligned)
		}
	}
}

// T4(b): eligibility cannot be removed from de-pegged slots to cherry-pick the clean ones: the lost coverage hands the verdict back to
// the larger of the aligned and whole-window gaps.
func TestQuoteSurface_IneligibleDepeggedSlotsStillRefuse(t *testing.T) {
	window := 24 * time.Hour
	cases := map[string]func(off time.Duration, tk *surfaceTick){
		"one own venue in the de-pegged slots": func(_ time.Duration, tk *surfaceTick) { tk.soleOwn = true },
		"disagreeing dust in the de-pegged slots": func(off time.Duration, tk *surfaceTick) {
			tk.extraPrice, tk.extraBase = basis(flat(off), 500), xlm(1)
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
// ineligible until own volume reaches 1/4 of proxy, so the attack must bring ~20 % of the window's proxy volume (~37 % of honest own
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
	if xNew != -1 && xNew < 15 {
		t.Errorf("a %d %%-of-proxy own-venue attack masked a 2 %% de-peg; want >= 15 %% (thin slots must not vouch)", xNew)
	}
}

// genSurfaceCase draws one 24 h case: a trend, a basis from a random slot, random hourly volume profiles and, in thin-evidence cases,
// quiet own-quote tails, one-venue slots and a third venue printing at the stablecoin price.
func genSurfaceCase(rng *rand.Rand) func(off time.Duration) surfaceTick {
	slope := []int64{-6, 0, 6}[rng.IntN(3)]
	depegFrom, depegBps := []int{0, rng.IntN(24)}[rng.IntN(2)], []int64{0, -150, 150, 400}[rng.IntN(4)]
	thin, shifted := rng.IntN(2) == 0, rng.IntN(2) == 0
	ownQuietFrom, proxyVols, ownVols := 24, []int64{250, 1000}, []int64{250, 1000}
	if thin {
		ownQuietFrom, proxyVols, ownVols = 24-rng.IntN(10), []int64{0, 250, 1000}, []int64{0, 50, 250, 1000}
	}
	proxyVol, ownVol := make([]int64, 24), make([]int64, 24)
	sole, extra := make([]bool, 24), make([]bool, 24)
	for s := range 24 {
		proxyVol[s], ownVol[s] = proxyVols[rng.IntN(len(proxyVols))], ownVols[rng.IntN(len(ownVols))]
		if shifted && !thin {
			proxyVol[s], ownVol[s] = 1000, 250
			if s >= 16 {
				proxyVol[s], ownVol[s] = 250, 1000
			}
		}
		sole[s], extra[s] = thin && rng.IntN(4) == 0, thin && rng.IntN(4) == 0
	}
	return func(off time.Duration) surfaceTick {
		s := int(off / time.Hour)
		price := new(big.Rat).Mul(big.NewRat(12, 100), big.NewRat(2400+slope*int64(s), 2400))
		tk := surfaceTick{proxyPrice: price, ownPrice: price, soleOwn: sole[s]}
		if s >= depegFrom {
			tk.proxyPrice = basis(price, depegBps)
		}
		if proxyVol[s] > 0 {
			tk.proxyBase = xlm(proxyVol[s])
		}
		if ownVol[s] > 0 && s < ownQuietFrom {
			tk.ownBase = xlm(ownVol[s])
		}
		if extra[s] {
			tk.extraPrice, tk.extraBase = tk.proxyPrice, xlm(ownVol[s]/10+1)
		}
		return tk
	}
}

// T5: over generated windows, in both coverage regimes, against the independent oracle:
//   - the reading decides alone exactly when eligible slots hold >= 95 % of proxy volume, and then refuses iff the proxy-weighted mean
//     gap exceeds 75 bps;
//   - below that coverage, a whole-window gap over 75 bps always refuses (never weaker than the old check);
//   - when every eligible slot's gap exceeds 75 bps with one sign, the leg is refused in either regime.
func TestQuoteSurface_GeneratedWindowsHoldTheDecisionRule(t *testing.T) {
	var alone, fallback, relieved, allOver int
	for seed := uint64(1); seed <= 400; seed++ {
		trades, proxied := buildSurface(t, 24*time.Hour, 10*time.Minute, genSurfaceCase(rand.New(rand.NewPCG(seed, 7))))
		want := surfaceOracleOf(trades, proxied)
		if want.whole == nil {
			continue
		}
		lr := readSurface(t, 24*time.Hour, trades, proxied)
		refused := lr.quoteSurfaceRefusal(surfaceTol) != ""
		if lr.surfaceAligned != want.decides || lr.surfaceOverlap != len(want.gaps) {
			t.Errorf("seed %d: aligned=%v overlap=%d, oracle decides=%v eligible=%d (coverage %s)",
				seed, lr.surfaceAligned, lr.surfaceOverlap, want.decides, len(want.gaps), want.coverage.FloatString(3))
			continue
		}
		if want.decides {
			alone++
			if exceeds(want.whole) && !refused {
				relieved++
			}
			if refused != exceeds(want.aligned) {
				t.Errorf("seed %d: decided alone, refused=%v, oracle aligned gap %.1f bps", seed, refused, ratBps(want.aligned))
			}
		} else {
			fallback++
			if exceeds(want.whole) && !refused {
				t.Errorf("seed %d: coverage %s, whole-window gap %.1f bps refuses but the leg passed at %.1f bps",
					seed, want.coverage.FloatString(3), ratBps(want.whole), gapBps(lr))
			}
		}
		if everyGapOverSameSign(want.gaps) {
			allOver++
			if !refused {
				t.Errorf("seed %d: every eligible slot over 75 bps with one sign, leg passed at %.1f bps", seed, gapBps(lr))
			}
		}
	}
	t.Logf("decided alone %d, fallback %d, trend relieved %d, all slots over %d", alone, fallback, relieved, allOver)
	if alone < 50 || fallback < 50 || relieved < 5 || allOver < 50 {
		t.Fatalf("generator coverage: decided alone %d, fallback %d, trend relieved %d, all slots over %d; want >= 50, 50, 5, 50",
			alone, fallback, relieved, allOver)
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
