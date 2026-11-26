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
// Bound when the aligned reading decides alone (coverage c >= 95 %): with eligible references that price their slot's proxy instants,
// |reading - D| <= (1 - c) * R * (1 + |d|), where D is the true proxy-weighted de-peg, R the window's relative own-quote price range
// (max/min - 1) and d the largest slot de-peg; the ineligible share counts in full, only its reference can be off, by at most R. Below 95 % the verdict is the larger of the
// reading and the whole-window gap (the whole-window gap alone with no eligible slot), never more lenient than the whole-window
// compare. Coverage is of the fetched proxy volume.
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

// slotGap is one proxy-bearing slot's signed gap and whether the slot is eligible (not [thinSlot], venues agree): eligible slots
// compare against their own-quote VWAP, the rest against ownWhole, which no print inside the slot can move. ok=false when a VWAP
// failed (the caller fails closed).
func (o *Orchestrator) slotGap(
	pair canonical.Pair, c *surfaceCell, floor *big.Int, maxBps int, ownWhole *big.Rat,
) (gap *big.Rat, eligible, ok bool) {
	proxyVWAP, err := o.computeNormalizedVWAP(c.proxy, pair)
	if err != nil || proxyVWAP.Sign() <= 0 {
		return nil, false, false
	}
	whole := relativeGap(proxyVWAP, ownWhole)
	if thinSlot(c, floor) {
		return whole, false, true
	}
	agree, ok := o.venuesAgree(pair, c, maxBps)
	if !ok {
		return nil, false, false
	}
	if !agree {
		return whole, false, true
	}
	all := make([]canonical.Trade, 0, len(c.own))
	for _, venueTrades := range c.own {
		all = append(all, venueTrades...)
	}
	ownVWAP, err := o.computeNormalizedVWAP(all, pair)
	if err != nil || ownVWAP.Sign() <= 0 {
		return nil, false, false
	}
	return relativeGap(proxyVWAP, ownVWAP), true, true
}

// alignedSurfaceGap is the mean of every proxy-bearing slot's [Orchestrator.slotGap] weighted by its proxy base, with eligible slots'
// count and share of proxy base; ok=false when a VWAP failed.
func (o *Orchestrator) alignedSurfaceGap(
	pair canonical.Pair, trades []canonical.Trade, sw aggregate.ScaledWindow, proxied map[string]struct{}, window time.Duration,
	proxyBase *big.Int, ownWhole *big.Rat, maxBps int,
) (reading alignedReading, ok bool) {
	sub := surfaceSubBucket(window)
	floor := surfaceQuoteFloor(sw.Decimals(), sub)
	weighted, covered := new(big.Rat), new(big.Int)
	for _, c := range surfaceCells(trades, sw.Trades(), proxied, sub) {
		if len(c.proxy) == 0 || c.proxyBase.Sign() <= 0 {
			continue
		}
		gap, eligible, computed := o.slotGap(pair, c, floor, maxBps, ownWhole)
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
	reading.decides = rest.Mul(rest, big.NewInt(surfaceCoverageDenom)).Cmp(proxyBase) <= 0
	return reading, true
}

// largerMagnitude returns whichever of a and b is larger in absolute value (a on a tie).
func largerMagnitude(a, b *big.Rat) *big.Rat {
	if new(big.Rat).Abs(b).Cmp(new(big.Rat).Abs(a)) > 0 {
		return b
	}
	return a
}
