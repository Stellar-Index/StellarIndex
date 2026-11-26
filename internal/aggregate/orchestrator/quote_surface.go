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
// It decides alone only at coverage c >= 95 %, |r| + (1-c)·R̂·(1+|r|) <= maxBps (R̂ = max/min - 1 over every slot's proxy and
// all-venue own-quote VWAP) and both surfaces over the window floor; else the verdict is the larger of r and the whole-window gap.
// Derivation: the quote-surface runbook.
//
// Floors are whole units per hour of slot (or window) length, pro rata; a partial edge slot must clear its full-length floor. Evidence
// is 720 production hours of XLM/USD (30 days; own = coinbase, kraken, bitstamp; proxy = binance USDT): p1 / p5 / p25 / p50 per hour.
// The values are the conservative candidate set, below every observed p1 so honest hours clear them; the runbook has both sets.
const (
	// surfaceSubBucketsPerWindow sets the slot at window/24 (5m→1m, 1h→2m, 24h→1h), never finer than closedBucket.
	surfaceSubBucketsPerWindow = 24
	// surfaceMinOwnSources: a slot's own-quote reference must come from this many qualifying venues, so one venue cannot set it alone.
	surfaceMinOwnSources = 2
	// surfaceVenueShareDenom: a venue qualifies only with >= 1/10 of the slot's own-quote base. Venue share: coinbase 24 / 34 / 50 /
	// 61 %, kraken 8 / 11 / 18 / 27 %, bitstamp 2 / 3 / 6 / 9 %; 2nd-largest venue 10 / 12 / 20 / 27 %.
	surfaceVenueShareDenom = 10
	// surfaceVenueQuoteFloorPerHour: a venue qualifies only with this much quote volume. Kraken 13.7k / 23k / 59k / 113k USD (min
	// 5.1k), bitstamp 6.9k / 9.4k / 19k / 33k.
	surfaceVenueQuoteFloorPerHour = 10_000
	// surfaceOwnShareDenom: the reference's base must be >= 1/3 of the slot's proxy base, so faking a reference costs volume in
	// proportion to the proxy volume it vouches for. Own/proxy base 0.46 / 0.69 / 1.16 / 1.62. 697-window replay, false alarms /
	// decided alone: 1/4 4 / 607, 1/3 7 / 571, 1/1 39 / 130 (main: 34 / 0).
	surfaceOwnShareDenom = 3
	// surfaceBaseFloorXLMPerHour: both surfaces must carry this much XLM base. Own 509k / 914k / 1.7M / 2.6M, proxy 426k / 562k /
	// 992k / 1.5M (min 183k). A leg whose base is not XLM has no floor and so never decides alone.
	surfaceBaseFloorXLMPerHour = 150_000
	// surfaceProxyQuoteFloorPerHour: the stablecoin surface's quote volume. Binance USDT 62k / 82k / 146k / 223k (min 27k).
	surfaceProxyQuoteFloorPerHour = 25_000
	// surfaceOwnQuoteFloorPerHour: the fiat reference's quote volume. Own USD 85k / 140k / 250k / 383k (min 52k). Quote units, not USD:
	// on a weak-currency leg (MXN) both quote floors are proportionally weaker; the base floor is not.
	surfaceOwnQuoteFloorPerHour = 25_000
	// surfaceWindowQuoteFloorPerHour: the aligned reading decides alone only when each surface's whole-window quote volume clears this.
	surfaceWindowQuoteFloorPerHour = 50_000
	// surfaceCoverageDenom: the aligned reading decides alone only when at most 1/20 (5 %) of the proxy volume lies in ineligible
	// slots, which bounds how far their less certain references can move it (see the bound above).
	surfaceCoverageDenom = 20
)

// surfaceFloor is perHour whole units over span at the given decimal scale, multiplied by one hour in nanoseconds so the pro-rata
// floor is exact: compare it with [underFloor].
func surfaceFloor(decimals int, perHour int64, span time.Duration) *big.Int {
	floor := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	return floor.Mul(floor, new(big.Int).Mul(big.NewInt(perHour), big.NewInt(int64(span))))
}

// underFloor reports v * 1h < floor (floor from [surfaceFloor]).
func underFloor(v, floor *big.Int) bool {
	return new(big.Int).Mul(v, big.NewInt(int64(time.Hour))).Cmp(floor) < 0
}

// surfaceFloors are one window's slot floors; nil when the leg's base is not XLM (no base floor, so no slot is eligible).
type surfaceFloors struct{ base, proxyQuote, ownQuote, venueQuote *big.Int }

func newSurfaceFloors(base canonical.Asset, decimals int, sub time.Duration) *surfaceFloors {
	for _, a := range canonical.AssetAliases(canonical.Asset{Type: canonical.AssetCrypto, Code: "XLM"}) {
		if a.Equal(base) {
			return &surfaceFloors{
				base:       surfaceFloor(decimals, surfaceBaseFloorXLMPerHour, sub),
				proxyQuote: surfaceFloor(decimals, surfaceProxyQuoteFloorPerHour, sub),
				ownQuote:   surfaceFloor(decimals, surfaceOwnQuoteFloorPerHour, sub),
				venueQuote: surfaceFloor(decimals, surfaceVenueQuoteFloorPerHour, sub),
			}
		}
	}
	return nil
}

// surfaceSubBucket is the time-alignment grain for window.
func surfaceSubBucket(window time.Duration) time.Duration {
	sub := (window / surfaceSubBucketsPerWindow).Truncate(closedBucket)
	if sub < closedBucket {
		return closedBucket
	}
	return sub
}

// surfaceVenue is one own-quote venue's prints in a slot, with its volume at the window's common scale.
type surfaceVenue struct {
	trades      []canonical.Trade
	base, quote *big.Int
}

// surfaceCell holds one slot's prints: proxy prints and their volume at the window's common scale, own-quote prints per source.
type surfaceCell struct {
	start                 time.Time
	proxy                 []canonical.Trade
	proxyBase, proxyQuote *big.Int
	ownBase               *big.Int
	own                   map[string]*surfaceVenue
}

// slotReference is what a slot's proxy prints are judged against: its qualifying own-quote venues, their summed volume and VWAP (nil
// with no qualifying venue). Venues outside it are ignored for eligibility, agreement and the slot's price.
type slotReference struct {
	venues      []*surfaceVenue
	base, quote *big.Int
	vwap        *big.Rat
}

// reference is the slot's qualifying own-quote venues (each >= 1/surfaceVenueShareDenom of the slot's own base and over the venue
// quote floor) in source order, unpriced; with nil floors no venue qualifies.
func (c *surfaceCell) reference(f *surfaceFloors) slotReference {
	ref := slotReference{base: new(big.Int), quote: new(big.Int)}
	if f == nil {
		return ref
	}
	names := make([]string, 0, len(c.own))
	for name := range c.own {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		v := c.own[name]
		if new(big.Int).Mul(v.base, big.NewInt(surfaceVenueShareDenom)).Cmp(c.ownBase) < 0 || underFloor(v.quote, f.venueQuote) {
			continue
		}
		ref.venues = append(ref.venues, v)
		ref.base.Add(ref.base, v.base)
		ref.quote.Add(ref.quote, v.quote)
	}
	return ref
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
				start: start, proxyBase: new(big.Int), proxyQuote: new(big.Int), ownBase: new(big.Int),
				own: make(map[string]*surfaceVenue),
			}
			byStart[start.UnixNano()] = c
		}
		if _, isProxy := proxied[trades[i].ID()]; isProxy {
			c.proxy = append(c.proxy, trades[i])
			c.proxyBase.Add(c.proxyBase, scaled[i].BaseAmount.BigInt())
			c.proxyQuote.Add(c.proxyQuote, scaled[i].QuoteAmount.BigInt())
			continue
		}
		v, ok := c.own[trades[i].Source]
		if !ok {
			v = &surfaceVenue{base: new(big.Int), quote: new(big.Int)}
			c.own[trades[i].Source] = v
		}
		v.trades = append(v.trades, trades[i])
		v.base.Add(v.base, scaled[i].BaseAmount.BigInt())
		v.quote.Add(v.quote, scaled[i].QuoteAmount.BigInt())
		c.ownBase.Add(c.ownBase, scaled[i].BaseAmount.BigInt())
	}
	cells := make([]*surfaceCell, 0, len(byStart))
	for _, c := range byStart {
		cells = append(cells, c)
	}
	sort.Slice(cells, func(i, j int) bool { return cells[i].start.Before(cells[j].start) })
	return cells
}

// thinSlot reports a slot whose evidence is too thin to vouch for its proxy prints: no floors (base not XLM), under
// surfaceMinOwnSources qualifying venues, reference base under 1/surfaceOwnShareDenom of proxy base, or either surface under its base
// or quote floor.
func thinSlot(c *surfaceCell, ref slotReference, f *surfaceFloors) bool {
	return f == nil || len(ref.venues) < surfaceMinOwnSources ||
		new(big.Int).Mul(ref.base, big.NewInt(surfaceOwnShareDenom)).Cmp(c.proxyBase) < 0 ||
		underFloor(ref.base, f.base) || underFloor(c.proxyBase, f.base) ||
		underFloor(ref.quote, f.ownQuote) || underFloor(c.proxyQuote, f.proxyQuote)
}

// relativeGap is (proxy - own) / own.
func relativeGap(proxy, own *big.Rat) *big.Rat {
	gap := new(big.Rat).Sub(proxy, own)
	return gap.Quo(gap, own)
}

// venuesAgree reports whether the reference venues' VWAPs lie within maxBps of the lowest (max - min <= maxBps of min), so a venue
// pricing away from the others cannot pull an eligible reference by more than maxBps; ok=false when a venue VWAP could not be computed.
func (o *Orchestrator) venuesAgree(pair canonical.Pair, venues []*surfaceVenue, maxBps int) (agree, ok bool) {
	var lo, hi *big.Rat
	for _, venue := range venues {
		v, err := o.computeNormalizedVWAP(venue.trades, pair)
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

// vwapOf prices trades, nil when there are none; ok=false when the VWAP failed or is not positive.
func (o *Orchestrator) vwapOf(pair canonical.Pair, trades []canonical.Trade) (*big.Rat, bool) {
	if len(trades) == 0 {
		return nil, true
	}
	v, err := o.computeNormalizedVWAP(trades, pair)
	if err != nil || v.Sign() <= 0 {
		return nil, false
	}
	return v, true
}

// slotVWAPs prices a slot's proxy prints, all its own-quote prints (the envelope sees every venue) and its reference (ref.vwap); nil
// for an absent side; ok=false when a VWAP failed.
func (o *Orchestrator) slotVWAPs(pair canonical.Pair, c *surfaceCell, ref *slotReference) (proxy, own *big.Rat, ok bool) {
	var all, referenced []canonical.Trade
	for _, v := range c.own {
		all = append(all, v.trades...)
	}
	for _, v := range ref.venues {
		referenced = append(referenced, v.trades...)
	}
	proxy, pok := o.vwapOf(pair, c.proxy)
	own, ook := o.vwapOf(pair, all)
	vwap, rok := o.vwapOf(pair, referenced)
	ref.vwap = vwap
	return proxy, own, pok && ook && rok
}

// slotGap is one proxy-bearing slot's signed gap and whether the slot is eligible (not [thinSlot], reference venues agree): eligible
// slots compare against their reference VWAP, the rest against ownWhole, which no print inside the slot can move. ok=false when a
// venue VWAP failed (the caller fails closed).
func (o *Orchestrator) slotGap(
	pair canonical.Pair, c *surfaceCell, proxyVWAP *big.Rat, ref slotReference, f *surfaceFloors, maxBps int, ownWhole *big.Rat,
) (gap *big.Rat, eligible, ok bool) {
	if ref.vwap == nil || thinSlot(c, ref, f) {
		return relativeGap(proxyVWAP, ownWhole), false, true
	}
	agree, ok := o.venuesAgree(pair, ref.venues, maxBps)
	if !ok {
		return nil, false, false
	}
	if !agree {
		return relativeGap(proxyVWAP, ownWhole), false, true
	}
	return relativeGap(proxyVWAP, ref.vwap), true, true
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

// windowClearsFloor reports whether both surfaces' whole-window quote volume clears surfaceWindowQuoteFloorPerHour over window.
func windowClearsFloor(cells []*surfaceCell, decimals int, window time.Duration) bool {
	proxyQuote, ownQuote := new(big.Int), new(big.Int)
	for _, c := range cells {
		proxyQuote.Add(proxyQuote, c.proxyQuote)
		for _, v := range c.own {
			ownQuote.Add(ownQuote, v.quote)
		}
	}
	floor := surfaceFloor(decimals, surfaceWindowQuoteFloorPerHour, window)
	return !underFloor(proxyQuote, floor) && !underFloor(ownQuote, floor)
}

// alignedSurfaceGap is the mean of every proxy-bearing slot's [Orchestrator.slotGap] weighted by its proxy base, with eligible slots'
// count and share of proxy base; it decides alone only at >= 95 % coverage, inside [withinBand] and over the window floor. ok=false
// when a VWAP failed.
func (o *Orchestrator) alignedSurfaceGap(
	pair canonical.Pair, trades []canonical.Trade, sw aggregate.ScaledWindow, proxied map[string]struct{}, window time.Duration,
	proxyBase *big.Int, ownWhole *big.Rat, maxBps int,
) (reading alignedReading, ok bool) {
	sub := surfaceSubBucket(window)
	floors := newSurfaceFloors(pair.Base, sw.Decimals(), sub)
	cells := surfaceCells(trades, sw.Trades(), proxied, sub)
	weighted, covered := new(big.Rat), new(big.Int)
	var env priceEnvelope
	for _, c := range cells {
		ref := c.reference(floors)
		proxyVWAP, ownVWAP, computed := o.slotVWAPs(pair, c, &ref)
		if !computed {
			return alignedReading{}, false
		}
		env.add(proxyVWAP)
		env.add(ownVWAP)
		if proxyVWAP == nil || c.proxyBase.Sign() <= 0 {
			continue
		}
		gap, eligible, computed := o.slotGap(pair, c, proxyVWAP, ref, floors, maxBps, ownWhole)
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
		withinBand(reading.gap, reading.coverage, env.relRange(), maxBps) && windowClearsFloor(cells, sw.Decimals(), window)
	return reading, true
}

// largerMagnitude returns whichever of a and b is larger in absolute value (a on a tie).
func largerMagnitude(a, b *big.Rat) *big.Rat {
	if new(big.Rat).Abs(b).Cmp(new(big.Rat).Abs(a)) > 0 {
		return b
	}
	return a
}
