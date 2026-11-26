package orchestrator

import (
	"math/big"
	"sort"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// The quote-surface check compares proxy and own-quote prints slot by slot, so a price trend under volume that falls at different
// times of day cannot read as a de-peg while a basis on the served stablecoin prints still does. The aligned reading is the mean of
// every proxy-bearing slot's gap weighted by its proxy base, i.e. the mean disagreement of exactly the prints the par rewrite vouches
// for. An eligible slot is judged against its own-quote VWAP; an ineligible one against the whole-window own-quote VWAP. Not the
// larger-magnitude of that and the slot's own prints: one print in a USD-silent slot could then pick a sign-flipped reading sized to
// cancel the eligible slots' de-peg.
//
// It decides alone only at coverage c >= 95 % and |r| + (1-c)·R̂·(1+|r|) <= maxBps, R̂ = max/min - 1 over every slot's proxy and
// own-quote VWAP (a ratio); else the verdict is the larger of r and the whole-window gap. Derivation: the quote-surface runbook.
const (
	// surfaceSubBucketsPerWindow sets the slot at window/24 (5m→1m, 1h→2m, 24h→1h), never finer than closedBucket.
	surfaceSubBucketsPerWindow = 24
	// surfaceMinOwnSources: a slot's own-quote reference must come from this many distinct venues, so one venue cannot set it alone.
	surfaceMinOwnSources = 2
	// surfaceOwnShareDenom: a slot's own-quote base must be >= 1/4 of its proxy base, so a slot carrying heavy proxy weight is never
	// judged against a thin reference and faking one costs volume in proportion to the proxy volume it would vouch for. Production
	// own-quote volume is ~1.6x proxy overall, so honest hours clear it with a wide margin.
	surfaceOwnShareDenom = 4
	// surfaceCoverageDenom: the aligned reading decides alone only when at most 1/20 (5 %) of the proxy volume lies in ineligible
	// slots, which bounds how far their less certain references can move it (see the bound above).
	surfaceCoverageDenom = 20
	// surfaceQuoteFloorPerHour: each surface of an eligible slot must carry >= this many whole quote-currency units per hour of slot
	// length (a partial edge slot must still clear its full-length floor), so two tiny surfaces never decide. Production XLM's
	// quietest hours carried ~40k USD own-quote and ~23k USD proxy volume. Quote units, not USD: on a weak-currency leg (MXN) it is
	// proportionally weaker, the venue, ratio and agreement rules still apply.
	surfaceQuoteFloorPerHour = 10_000
)

// surfaceQuoteFloor is surfaceQuoteFloorPerHour for a slot of length sub at the given decimal scale, multiplied by one hour in
// nanoseconds so the pro-rata floor is exact: compare it against quote volume times int64(time.Hour).
func surfaceQuoteFloor(decimals int, sub time.Duration) *big.Int {
	floor := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	return floor.Mul(floor, big.NewInt(surfaceQuoteFloorPerHour*int64(sub)))
}

// underFloor reports quote * 1h < floor (floor from [surfaceQuoteFloor]).
func underFloor(quote, floor *big.Int) bool {
	return new(big.Int).Mul(quote, big.NewInt(int64(time.Hour))).Cmp(floor) < 0
}

// surfaceSubBucket is the time-alignment grain for window.
func surfaceSubBucket(window time.Duration) time.Duration {
	sub := (window / surfaceSubBucketsPerWindow).Truncate(closedBucket)
	if sub < closedBucket {
		return closedBucket
	}
	return sub
}

// surfaceCell holds one slot's prints: proxy prints and their base volume at the window's common scale, own-quote prints per source.
type surfaceCell struct {
	start                 time.Time
	proxy                 []canonical.Trade
	proxyBase, proxyQuote *big.Int
	ownBase, ownQuote     *big.Int
	own                   map[string][]canonical.Trade
}

// alignedReading is the time-aligned comparison: gap is the proxy-weighted mean of every proxy-bearing slot's gap (nil with no
// eligible slot, so the whole-window compare judges alone, exactly as before alignment), coverage the share
// of the window's proxy base volume in eligible slots, decides whether that share lets gap stand alone.
type alignedReading struct {
	gap      *big.Rat
	eligible int
	coverage *big.Rat
	decides  bool
}

// surfaceCells buckets trades by slot start (absolute-time aligned, so input order cannot move a boundary), oldest first; scaled is
// trades at the common amount scale, index-aligned.
func surfaceCells(trades, scaled []canonical.Trade, proxied map[string]struct{}, sub time.Duration) []*surfaceCell {
	byStart := make(map[int64]*surfaceCell)
	for i := range trades {
		start := trades[i].Timestamp.UTC().Truncate(sub)
		c, ok := byStart[start.UnixNano()]
		if !ok {
			c = &surfaceCell{
				start: start, proxyBase: new(big.Int), proxyQuote: new(big.Int), ownBase: new(big.Int), ownQuote: new(big.Int),
				own: make(map[string][]canonical.Trade),
			}
			byStart[start.UnixNano()] = c
		}
		if _, isProxy := proxied[trades[i].ID()]; isProxy {
			c.proxy = append(c.proxy, trades[i])
			c.proxyBase.Add(c.proxyBase, scaled[i].BaseAmount.BigInt())
			c.proxyQuote.Add(c.proxyQuote, scaled[i].QuoteAmount.BigInt())
			continue
		}
		c.own[trades[i].Source] = append(c.own[trades[i].Source], trades[i])
		c.ownBase.Add(c.ownBase, scaled[i].BaseAmount.BigInt())
		c.ownQuote.Add(c.ownQuote, scaled[i].QuoteAmount.BigInt())
	}
	cells := make([]*surfaceCell, 0, len(byStart))
	for _, c := range byStart {
		cells = append(cells, c)
	}
	sort.Slice(cells, func(i, j int) bool { return cells[i].start.Before(cells[j].start) })
	return cells
}

// thinSlot reports a slot whose evidence is too thin to vouch for its proxy prints: under surfaceMinOwnSources own venues, own base
// under 1/surfaceOwnShareDenom of proxy base, or either surface's quote volume under floor.
func thinSlot(c *surfaceCell, floor *big.Int) bool {
	return len(c.own) < surfaceMinOwnSources ||
		new(big.Int).Mul(c.ownBase, big.NewInt(surfaceOwnShareDenom)).Cmp(c.proxyBase) < 0 ||
		underFloor(c.ownQuote, floor) || underFloor(c.proxyQuote, floor)
}

// relativeGap is (proxy - own) / own.
func relativeGap(proxy, own *big.Rat) *big.Rat {
	gap := new(big.Rat).Sub(proxy, own)
	return gap.Quo(gap, own)
}

// venuesAgree reports whether the slot's venue VWAPs lie within maxBps of the lowest (max - min <= maxBps of min), so a venue pricing
// away from the others cannot pull an eligible reference by more than maxBps; ok=false when a venue VWAP could not be computed.
func (o *Orchestrator) venuesAgree(pair canonical.Pair, c *surfaceCell, maxBps int) (agree, ok bool) {
	var lo, hi *big.Rat
	for _, venueTrades := range c.own {
		v, err := o.computeNormalizedVWAP(venueTrades, pair)
		if err != nil || v.Sign() <= 0 {
			return false, false
		}
		if lo == nil || v.Cmp(lo) < 0 {
			lo = v
		}
		if hi == nil || v.Cmp(hi) > 0 {
			hi = v
		}
	}
	spread := new(big.Rat).Sub(hi, lo)
	return spread.Mul(spread, big.NewRat(10_000, 1)).Cmp(new(big.Rat).Mul(lo, big.NewRat(int64(maxBps), 1))) <= 0, true
}

// slotVWAPs prices a slot's proxy prints and all its own-quote prints (nil for an absent side); ok=false when a VWAP failed.
func (o *Orchestrator) slotVWAPs(pair canonical.Pair, c *surfaceCell) (proxy, own *big.Rat, ok bool) {
	if len(c.proxy) > 0 {
		v, err := o.computeNormalizedVWAP(c.proxy, pair)
		if err != nil || v.Sign() <= 0 {
			return nil, nil, false
		}
		proxy = v
	}
	if len(c.own) > 0 {
		all := make([]canonical.Trade, 0, len(c.own))
		for _, venueTrades := range c.own {
			all = append(all, venueTrades...)
		}
		v, err := o.computeNormalizedVWAP(all, pair)
		if err != nil || v.Sign() <= 0 {
			return nil, nil, false
		}
		own = v
	}
	return proxy, own, true
}

// slotGap is one proxy-bearing slot's signed gap and whether the slot is eligible (not [thinSlot], venues agree): eligible slots
// compare against their own-quote VWAP, the rest against ownWhole, which no print inside the slot can move. ok=false when a venue
// VWAP failed (the caller fails closed).
func (o *Orchestrator) slotGap(
	pair canonical.Pair, c *surfaceCell, proxyVWAP, ownVWAP *big.Rat, floor *big.Int, maxBps int, ownWhole *big.Rat,
) (gap *big.Rat, eligible, ok bool) {
	if ownVWAP == nil || thinSlot(c, floor) {
		return relativeGap(proxyVWAP, ownWhole), false, true
	}
	agree, ok := o.venuesAgree(pair, c, maxBps)
	if !ok {
		return nil, false, false
	}
	if !agree {
		return relativeGap(proxyVWAP, ownWhole), false, true
	}
	return relativeGap(proxyVWAP, ownVWAP), true, true
}

// priceEnvelope is the lowest and highest slot VWAP seen on either surface.
type priceEnvelope struct{ lo, hi *big.Rat }

func (e *priceEnvelope) add(v *big.Rat) {
	if v == nil {
		return
	}
	if e.lo == nil || v.Cmp(e.lo) < 0 {
		e.lo = v
	}
	if e.hi == nil || v.Cmp(e.hi) > 0 {
		e.hi = v
	}
}

// relRange is hi/lo - 1, the window's relative price range R̂ (0 when empty).
func (e priceEnvelope) relRange() *big.Rat {
	if e.lo == nil {
		return new(big.Rat)
	}
	r := new(big.Rat).Quo(e.hi, e.lo)
	return r.Sub(r, big.NewRat(1, 1))
}

// withinBand reports |r| + (1 - c) * rng * (1 + |r|) <= maxBps / 10_000: the reading stays inside the tolerance even if the
// ineligible share's references are off by the whole observed range.
func withinBand(r, c, rng *big.Rat, maxBps int) bool {
	abs := new(big.Rat).Abs(r)
	band := new(big.Rat).Sub(big.NewRat(1, 1), c)
	band.Mul(band, rng).Mul(band, new(big.Rat).Add(big.NewRat(1, 1), abs))
	return band.Add(band, abs).Cmp(big.NewRat(int64(maxBps), 10_000)) <= 0
}

// alignedSurfaceGap is the mean of every proxy-bearing slot's [Orchestrator.slotGap] weighted by its proxy base, with eligible slots'
// count and share of proxy base; it decides alone only at >= 95 % coverage and inside [withinBand]. ok=false when a VWAP failed.
func (o *Orchestrator) alignedSurfaceGap(
	pair canonical.Pair, trades []canonical.Trade, sw aggregate.ScaledWindow, proxied map[string]struct{}, window time.Duration,
	proxyBase *big.Int, ownWhole *big.Rat, maxBps int,
) (reading alignedReading, ok bool) {
	sub := surfaceSubBucket(window)
	floor := surfaceQuoteFloor(sw.Decimals(), sub)
	weighted, covered := new(big.Rat), new(big.Int)
	var env priceEnvelope
	for _, c := range surfaceCells(trades, sw.Trades(), proxied, sub) {
		proxyVWAP, ownVWAP, computed := o.slotVWAPs(pair, c)
		if !computed {
			return alignedReading{}, false
		}
		env.add(proxyVWAP)
		env.add(ownVWAP)
		if proxyVWAP == nil || c.proxyBase.Sign() <= 0 {
			continue
		}
		gap, eligible, computed := o.slotGap(pair, c, proxyVWAP, ownVWAP, floor, maxBps, ownWhole)
		if !computed {
			return alignedReading{}, false
		}
		weighted.Add(weighted, gap.Mul(gap, new(big.Rat).SetInt(c.proxyBase)))
		if eligible {
			covered.Add(covered, c.proxyBase)
			reading.eligible++
		}
	}
	reading.coverage = new(big.Rat).SetFrac(covered, proxyBase)
	if reading.eligible == 0 {
		return reading, true
	}
	reading.gap = weighted.Quo(weighted, new(big.Rat).SetInt(proxyBase))
	rest := new(big.Int).Sub(proxyBase, covered)
	reading.decides = rest.Mul(rest, big.NewInt(surfaceCoverageDenom)).Cmp(proxyBase) <= 0 &&
		withinBand(reading.gap, reading.coverage, env.relRange(), maxBps)
	return reading, true
}

// largerMagnitude returns whichever of a and b is larger in absolute value (a on a tie).
func largerMagnitude(a, b *big.Rat) *big.Rat {
	if new(big.Rat).Abs(b).Cmp(new(big.Rat).Abs(a)) > 0 {
		return b
	}
	return a
}
