package v1

import (
	"context"
	"math/big"
	"sort"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
)

// ohlcSeriesFiatCombined builds a fiat-quoted OHLC series by combining every USD-pegged
// constituent per bucket. `fiat:USD` itself has only recent CEX trades; the deep history sits
// under the stablecoin pairs, and using the aggregator's own set keeps bars consistent with /v1/price.
//
// Per bucket the combine is exact in big.Rat: volumes summed, high/low max/min, open/close
// base-volume-weighted. Source scales differ (DEX 7dp, CEX 8, FX 6), so each bar is lifted to the
// maximum scale first (ADR-0003), read from the bar's own [OHLCSeriesBar.Sources].
//
// Held-back constituents (a declared peg's SAC wrapper) fill only buckets no established spelling
// answered, because a thin Soroban print merged into a book-backed bucket can move its high. Gating
// per bucket keeps a day rendering the same whatever window it is read in
// (docs/architecture/aggregation-plan.md, section 7.5). The bool is `proxied` (a bar's quote leg
// differs from `pair.Quote`); it drives `flags.triangulated`.
func (s *Server) ohlcSeriesFiatCombined(
	ctx context.Context,
	pair canonical.Pair,
	interval ohlcInterval,
	from, to time.Time,
	limit int,
) ([]OHLCSeriesBar, bool, error) {
	established, heldBack := s.usdPeggedConstituentSets(pair)

	c := newFiatCombine()
	estProxied, err := s.combineConstituentBars(ctx, c, established, pair, interval, from, to, limit, nil)
	if err != nil {
		return nil, false, err
	}
	// Which buckets the established spellings answered, snapshotted
	// BEFORE the held-back pass so two held-back constituents sharing an
	// unanswered bucket still merge with each other.
	answered := make(map[time.Time]struct{}, len(c.acc))
	for t := range c.acc {
		answered[t] = struct{}{}
	}
	unanswered := func(t time.Time) bool {
		_, taken := answered[t]
		return !taken
	}
	heldBackProxied, err := s.combineConstituentBars(ctx, c, heldBack, pair, interval, from, to, limit, unanswered)
	if err != nil {
		return nil, false, err
	}
	acc := c.acc
	if len(acc) == 0 {
		return nil, false, nil
	}

	out := make([]OHLCSeriesBar, 0, len(acc))
	for t, a := range acc {
		out = append(out, a.finalize(t))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].T.Time().Before(out[j].T.Time()) })
	return newestCombinedBars(out, limit), estProxied || heldBackProxied, nil
}

// newestCombinedBars keeps the NEWEST `limit` of an ascending merged series; which end survives
// decides whether served bars are correct.
//
// Each constituent read is capped to its own newest `limit` buckets, so the merge is a union of
// per-constituent tails: the old end holds buckets a dense constituent traded in but was never
// asked for, and keeping the earliest `limit` would serve incomplete bars. The newest `limit` are
// safe by counting: a constituent has at most `limit` buckets at or after any such t, all inside its
// own newest `limit`. The same count covers the held-back gate. Only bites on an explicit window
// wider than `limit` intervals.
func newestCombinedBars(out []OHLCSeriesBar, limit int) []OHLCSeriesBar {
	if limit > 0 && len(out) > limit {
		return out[len(out)-limit:]
	}
	return out
}

// usdPeggedConstituents is the concatenation of [Server.usdPeggedConstituentSets], established
// spellings first. The coverage probe measures it ([Server.ohlcCoverageSet]) because a floor is
// only consulted on an empty answer, and an empty answer means BOTH sets were read.
func (s *Server) usdPeggedConstituents(pair canonical.Pair) []canonical.Pair {
	established, heldBack := s.usdPeggedConstituentSets(pair)
	out := make([]canonical.Pair, 0, len(established)+len(heldBack))
	out = append(out, established...)
	return append(out, heldBack...)
}

// usdPeggedConstituentSets partitions a fiat-quoted target's cagg pairs into the set that is
// COMBINED and the set read only where the first left a bucket unanswered.
//
// `established` is the aggregator's own set: every XLM dual-form base alias crossed with the
// USD-peg expansion. `heldBack` is the remaining canonical form of each quote, in practice a
// declared classic peg's SAC wrapper. A peg is an asset, not a spelling: Soroban AMMs trade the
// wrapper, so a token whose only USD depth is a Soroban pool is stored quoted in the USDC SAC.
//
// The passes are [Server.usdPegProxyQuotes]'s classic-then-SAC across ALL peg families, because a
// pool is far thinner than the book and a few prints must not re-price an answer the book can give
// (docs/architecture/aggregation-plan.md, "The fiat quote leg, per bucket").
//
// Deduplicated by market across both sets, since both readers span both stored directions; a
// held-back pair whose flip is established is dropped, not promoted.
func (s *Server) usdPeggedConstituentSets(pair canonical.Pair) (established, heldBack []canonical.Pair) {
	seen := make(map[string]struct{})
	add := func(dst *[]canonical.Pair, sp canonical.Pair) {
		k := sp.Base.String() + "\x00" + sp.Quote.String()
		if _, dup := seen[k]; dup {
			return
		}
		seen[k] = struct{}{}
		*dst = append(*dst, sp)
	}
	for _, b := range assetAliases(pair.Base) {
		tgt, err := canonical.NewPair(b, pair.Quote)
		if err != nil {
			continue
		}
		expanded, err := aggregate.ExpandTargetPairWithClassicPegs(tgt, s.USDPeggedClassics)
		if err != nil {
			// Malformed target — fall back to the direct pair only.
			expanded = []canonical.Pair{tgt}
		}
		for _, sp := range expanded {
			add(&established, sp)
			for _, form := range assetAliases(sp.Quote) {
				if form.Equal(sp.Quote) {
					continue // the spelling the expansion already named
				}
				alt, aerr := canonical.NewPair(sp.Base, form)
				if aerr != nil {
					continue // degenerate combination (one asset against itself)
				}
				add(&heldBack, alt)
			}
		}
	}
	established = distinctMarkets(established)
	return established, dropMarketsIn(distinctMarkets(heldBack), established)
}

// dropMarketsIn removes from `pairs` every market already in `kept`, in either direction. It keeps
// the held-back set disjoint from the established one: a market reachable under both belongs to
// the set read first.
func dropMarketsIn(pairs, kept []canonical.Pair) []canonical.Pair {
	if len(pairs) == 0 || len(kept) == 0 {
		return pairs
	}
	out := make([]canonical.Pair, 0, len(pairs))
	for _, p := range pairs {
		dup := false
		for _, k := range kept {
			if k.EqualEitherWay(p) {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, p)
		}
	}
	return out
}

// fiatCombinedTrades is the point-path twin of [Server.ohlcSeriesFiatCombined]: it merges the raw
// trades of EVERY constituent of a fiat-quoted target into one chronologically-ordered population
// at one amount scale. Returns (window, proxied, err); `proxied` drives flags.triangulated.
//
// Backing a point quote with one constituent while the series combines all would make
// `/v1/vwap?quote=fiat:USD` and `/v1/ohlc?interval=1h&quote=fiat:USD` answer from different
// populations; the series methodology is the authority. The held-back gate resolves at
// [fiatPointGateInterval], the finest interval the series accepts, so equality is exact against
// `interval=1m`; a coarser series suppresses more.
//
// Constituent read errors propagate: a quietly narrower money answer is worse than a 500. Each is
// fetched with the caller's `maxTrades` cap and the merged set is trimmed to the newest
// `maxTrades`, preserving the callers' `len(trades) == maxTrades` truncation signal.
func (s *Server) fiatCombinedTrades(
	ctx context.Context, pair canonical.Pair, from, to time.Time, maxTrades int,
) (aggregate.ScaledWindow, bool, error) {
	established, heldBack := s.usdPeggedConstituentSets(pair)
	merged, proxied, err := s.mergeConstituentTrades(ctx, established, pair, from, to, maxTrades)
	if err != nil {
		return aggregate.ScaledWindow{}, false, err
	}
	// Held-back spellings answer only the sub-windows the established ones left empty. Gating on the
	// whole window would make the point window its own bucket: with the book trading in the first
	// hour and the pool in the second, the series carries both while /v1/vwap carried only the book.
	if len(heldBack) > 0 {
		answered := tradeGateBuckets(merged)
		for _, sp := range heldBack {
			batch, berr := s.History.TradesInRange(ctx, sp, from, to, maxTrades)
			if berr != nil {
				return aggregate.ScaledWindow{}, false, berr
			}
			// `answered` is snapshotted from the established rows before any held-back row joins,
			// so two held-back constituents sharing an empty bucket still merge, as in the series.
			batch = dropTradesInAnsweredBuckets(batch, answered)
			if len(batch) == 0 {
				continue
			}
			if !sp.Quote.Equal(pair.Quote) {
				proxied = true
			}
			merged = append(merged, batch...)
		}
	}
	sortTradesChronological(merged)
	if maxTrades > 0 && len(merged) > maxTrades {
		merged = merged[len(merged)-maxTrades:]
	}
	// Lift every trade to a common smallest-unit scale: constituents mix on-chain (7dp) and CEX (8dp)
	// legs, and sum(quote)/sum(base) over-weights the finer source ~10x per decimal of difference.
	// Uniform windows come back byte-identical; TWAP is scale-invariant, so this is a no-op there.
	return aggregate.NormalizeAmountScale(merged, amountScaleDecimalsFor), proxied, nil
}

// mergeConstituentTrades reads one constituent set into a single population and reports whether
// any constituent's quote leg proxied the requested one. Read errors propagate, as in
// [Server.fiatCombinedTrades].
func (s *Server) mergeConstituentTrades(
	ctx context.Context, pairs []canonical.Pair, pair canonical.Pair, from, to time.Time, maxTrades int,
) ([]canonical.Trade, bool, error) {
	var merged []canonical.Trade
	proxied := false
	for _, sp := range pairs {
		batch, err := s.History.TradesInRange(ctx, sp, from, to, maxTrades)
		if err != nil {
			return nil, false, err
		}
		if len(batch) == 0 {
			continue
		}
		// Triangulated means the QUOTE leg was proxied (X/fiat:USD served from X/USDC). A base-alias
		// constituent (`crypto:XLM/fiat:USD` for `?base=native`) is the same asset, not a proxy.
		if !sp.Quote.Equal(pair.Quote) {
			proxied = true
		}
		merged = append(merged, batch...)
	}
	return merged, proxied, nil
}

// fiatPointGateInterval is the bucket grain the point path gates held-back constituents at: the
// finest interval `/v1/ohlc` accepts, which makes "point == series" exact for a real request.
const fiatPointGateInterval = ohlcInterval1m

// tradeGateBuckets is the set of gate buckets the given trades occupy.
func tradeGateBuckets(trades []canonical.Trade) map[time.Time]struct{} {
	if len(trades) == 0 {
		return nil
	}
	out := make(map[time.Time]struct{}, len(trades))
	for i := range trades {
		out[fiatPointGateBucket(trades[i].Timestamp)] = struct{}{}
	}
	return out
}

// dropTradesInAnsweredBuckets removes every trade whose gate bucket an
// established spelling already answered. Returns the input untouched
// when nothing was answered, which is the pool-only asset — the case
// where the alternative is no price at all.
func dropTradesInAnsweredBuckets(trades []canonical.Trade, answered map[time.Time]struct{}) []canonical.Trade {
	if len(answered) == 0 || len(trades) == 0 {
		return trades
	}
	out := trades[:0:0]
	for i := range trades {
		if _, taken := answered[fiatPointGateBucket(trades[i].Timestamp)]; taken {
			continue
		}
		out = append(out, trades[i])
	}
	return out
}

// fiatPointGateBucket floors a trade's timestamp onto the gate grid.
// UTC first: [time.Time.Truncate] works on the absolute instant, but the
// bucket is compared as a value, and two identical instants carrying
// different locations are not equal.
func fiatPointGateBucket(ts time.Time) time.Time {
	return ts.UTC().Truncate(fiatPointGateInterval.duration())
}

// amountScaleDecimalsFor resolves a trade source's smallest-unit scale from the external registry
// (8 CEX/aggregator, 7 on-chain DEX, 6 FX). It is a package-level func so
// aggregate.NormalizeAmountScale avoids importing external, which imports aggregate (a cycle).
func amountScaleDecimalsFor(source string) int {
	return external.Lookup(source).AmountScaleDecimals()
}

// sortTradesChronological orders a merged trade set by close time ascending, which
// [aggregate.ComputeOHLC] and [aggregate.TWAP] require (they take open/close and time weights from
// slice order). Ties break by [aggregate.TradeOrderLess], not slice position, so the served
// open/close never depends on merge order (ADR-0015: all regions return the same rate).
func sortTradesChronological(trades []canonical.Trade) {
	sort.Slice(trades, func(i, j int) bool {
		return aggregate.TradeOrderLess(&trades[i], &trades[j])
	})
}

// ohlcBarScaleUnknown is the scale of a bar whose reader reported no sources. It is a distinct value,
// not a plausible default: calling it 8 (the registry fallback) would reintroduce the 10x mis-weight
// this file corrects. It lifts by a factor of 1, so it leaves a bar where it was.
const ohlcBarScaleUnknown = -1

// barScaleDecimals resolves a combined bar's smallest-unit scale from its contributing venues, via
// the same resolver the point path hands to [aggregate.NormalizeAmountScale].
//
// The maximum is taken. Stored bars are homogeneous in scale, so the lift is exact; a bar that mixed
// scales is already an unrepairable sum, and max gives it the smallest lift so it cannot inflate its
// weight against peers. The scale is unknown when any source lacks an [external.Registry] entry,
// because [external.Lookup] would answer the CEX-flavoured 8-decimal default and misstate an
// unregistered on-chain DEX tenfold. Mirrors [commonAmountScaleDecimals], the point-path twin.
func barScaleDecimals(sources []string) int {
	scale := ohlcBarScaleUnknown
	for _, src := range sources {
		if !external.Registered(src) {
			return ohlcBarScaleUnknown
		}
		if d := amountScaleDecimalsFor(src); d > scale {
			scale = d
		}
	}
	return scale
}

// ohlcScaleFactor is the exact integer multiplier lifting a bar at `scale` to `common`. The common
// scale is the set maximum, so nothing is ever divided; it returns 1 for an equal or unknown scale.
func ohlcScaleFactor(scale, common int) *big.Rat {
	if scale == ohlcBarScaleUnknown || common == ohlcBarScaleUnknown || common <= scale {
		return new(big.Rat).SetInt64(1)
	}
	exp := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(common-scale)), nil)
	return new(big.Rat).SetInt(exp)
}

// fiatCombine is the running state of one fiat-combined response: one accumulator per bucket, each
// carrying its own scale.
//
// The lift target is per bucket, not per response: a bucket's numbers must depend only on its own
// bars, or the same day renders differently by window width. The cost is that two buckets of one
// response can be in different scales, but a response-wide scale would show a caller stitching two
// windows (a paged chart, an incremental refresh) the same bucket in two units.
type fiatCombine struct {
	acc map[time.Time]*ohlcBucketAcc
}

func newFiatCombine() *fiatCombine {
	return &fiatCombine{acc: make(map[time.Time]*ohlcBucketAcc, 256)}
}

func (c *fiatCombine) add(b *OHLCSeriesBar) {
	a := c.acc[b.T.Time()]
	if a == nil {
		a = newOHLCBucketAcc()
		c.acc[b.T.Time()] = a
	}
	a.add(b, barScaleDecimals(b.Sources))
}

// combineConstituentBars reads each pair's bars and folds them into `c`. A non-nil `admit` decides
// per bucket whether a bar may contribute, confining the held-back pass to buckets the established
// pass left unanswered; nil takes every bar.
//
// Read errors propagate: dropping one would silently narrow a money answer, which is worse than a
// 500 ([Server.fiatCombinedTrades]). Returns `proxied`, true when an admitted bar's quote leg
// differs from `pair`'s, the twin of [Server.mergeConstituentTrades]' flag.
func (s *Server) combineConstituentBars(
	ctx context.Context,
	c *fiatCombine,
	pairs []canonical.Pair,
	pair canonical.Pair,
	interval ohlcInterval,
	from, to time.Time,
	limit int,
	admit func(time.Time) bool,
) (bool, error) {
	proxied := false
	for _, sp := range pairs {
		bars, err := s.History.OHLCSeries(ctx, sp, string(interval), from, to, limit)
		if err != nil {
			// A constituent that simply has no rows returns an empty
			// slice + nil, not an error.
			return false, err
		}
		for i := range bars {
			if admit != nil && !admit(bars[i].T.Time()) {
				continue
			}
			c.add(&bars[i])
			if !sp.Quote.Equal(pair.Quote) {
				proxied = true
			}
		}
	}
	return proxied, nil
}

// ohlcBucketAcc accumulates constituent bars sharing a bucket timestamp, in big.Rat/big.Int to keep
// the NUMERIC precision of the wire contract (no float round-trip).
//
// Volume sums are kept per scale, because the lift target is the bucket's maximum scale, unknown
// until every constituent is read. Prices are scale-invariant ratios, so extremes and the trade
// count need no such split.
type ohlcBucketAcc struct {
	byScale map[int]*ohlcScaleAcc // keyed by the contributing bars' scale
	highs   []*big.Rat            // per-constituent highs (bucket high = max)
	lows    []*big.Rat            // per-constituent lows  (bucket low  = min)
	n       int64
	sources map[string]struct{} // venues behind the bucket; served as `sources`
	// commonScale is the finest scale any bar admitted to this bucket declares: the lift target, so
	// every lift is an exact multiply by 10^(common−scale) ≥ 1 (ADR-0003). It is bucket-scoped so
	// the bar depends on no window. See [fiatCombine].
	commonScale int
}

// ohlcScaleAcc is one bucket's running totals for the bars at a single
// smallest-unit scale.
type ohlcScaleAcc struct {
	openNum  *big.Rat // Σ(open  · base_vol)
	closeNum *big.Rat // Σ(close · base_vol)
	baseVol  *big.Rat // Σ base_vol (weight denominator)
	quoteVol *big.Rat // Σ quote_vol
}

func newOHLCBucketAcc() *ohlcBucketAcc {
	return &ohlcBucketAcc{byScale: make(map[int]*ohlcScaleAcc, 2), sources: map[string]struct{}{}, commonScale: ohlcBarScaleUnknown}
}

func (a *ohlcBucketAcc) add(b *OHLCSeriesBar, scale int) {
	open := ratFromDecimal(b.O)
	closeP := ratFromDecimal(b.C)
	high := ratFromDecimal(b.H)
	low := ratFromDecimal(b.L)
	bv := ratFromDecimal(b.VBase)
	qv := ratFromDecimal(b.VQuote)
	if open == nil || closeP == nil || high == nil || low == nil || bv == nil {
		return // unparseable row — skip rather than corrupt the bucket
	}

	if scale > a.commonScale {
		a.commonScale = scale
	}
	sa := a.byScale[scale]
	if sa == nil {
		sa = &ohlcScaleAcc{
			openNum:  new(big.Rat),
			closeNum: new(big.Rat),
			baseVol:  new(big.Rat),
			quoteVol: new(big.Rat),
		}
		a.byScale[scale] = sa
	}
	sa.openNum.Add(sa.openNum, new(big.Rat).Mul(open, bv))
	sa.closeNum.Add(sa.closeNum, new(big.Rat).Mul(closeP, bv))
	sa.baseVol.Add(sa.baseVol, bv)
	if qv != nil {
		sa.quoteVol.Add(sa.quoteVol, qv)
	}
	a.n += b.N
	for _, src := range b.Sources {
		a.sources[src] = struct{}{}
	}
	// Collect the per-constituent extremes; the bucket extreme is simply the
	// max/min across them (each constituent's own extreme already excludes
	// dust — see selectExtreme).
	a.highs = append(a.highs, high)
	a.lows = append(a.lows, low)
}

// finalize renders the bucket, lifting each scale's totals to the bucket's common scale so volumes
// and weighted open/close share one unit, stated as v_base_decimals/v_quote_decimals.
//
// Per-scale totals are summed in map-iteration order, safe only because exact big.Rat addition is
// order-independent. The point path differs (slice order picks open/close), so
// [sortTradesChronological] imposes a total order for ADR-0015.
func (a *ohlcBucketAcc) finalize(t time.Time) OHLCSeriesBar {
	openNum, closeNum := new(big.Rat), new(big.Rat)
	baseVol, quoteVol := new(big.Rat), new(big.Rat)
	for scale, sa := range a.byScale {
		f := ohlcScaleFactor(scale, a.commonScale)
		openNum.Add(openNum, new(big.Rat).Mul(sa.openNum, f))
		closeNum.Add(closeNum, new(big.Rat).Mul(sa.closeNum, f))
		baseVol.Add(baseVol, new(big.Rat).Mul(sa.baseVol, f))
		quoteVol.Add(quoteVol, new(big.Rat).Mul(sa.quoteVol, f))
	}

	open, closeP := new(big.Rat), new(big.Rat)
	if baseVol.Sign() > 0 {
		open.Quo(openNum, baseVol)
		closeP.Quo(closeNum, baseVol)
	}
	high := selectExtreme(a.highs, true)
	low := selectExtreme(a.lows, false)
	// A bucket holding an unknown-scale bar sums integers at no single
	// scale (that bar was left unlifted), so it states none.
	stated := a.commonScale
	if _, unknown := a.byScale[ohlcBarScaleUnknown]; unknown {
		stated = ohlcBarScaleUnknown
	}
	return OHLCSeriesBar{
		T:      WireTime(t),
		O:      ratToDecimal(open, ohlcPriceDigits),
		H:      ratToDecimal(high, ohlcPriceDigits),
		L:      ratToDecimal(low, ohlcPriceDigits),
		C:      ratToDecimal(closeP, ohlcPriceDigits),
		VBase:  seriesVolumeText(baseVol),
		VQuote: seriesVolumeText(quoteVol),
		// The bucket's own lift target IS the scale VBase/VQuote ended
		// up at — every scale present was multiplied up to it above, so
		// stating anything else would mismatch the served integers.
		VBaseDecimals:  wireScaleDecimals(stated),
		VQuoteDecimals: wireScaleDecimals(stated),
		N:              a.n,
		Sources:        sortedSources(a.sources),
	}
}

// sortedSources renders a source set in lexical order so a bucket's
// provenance is a function of its bars, never of map iteration.
func sortedSources(set map[string]struct{}) []string {
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for src := range set {
		out = append(out, src)
	}
	sort.Strings(out)
	return out
}

// selectExtreme returns the bucket high (isHigh) or low across per-constituent extremes: plain
// max / min with no price-distance filtering.
//
// A price band around the VWAP is the wrong filter
// (docs/operations/finding-dust-trades-set-chart-extremes.md):
// the wicks it targets are dust, already excluded per trade by the $0.01 notional floor on the CAGG
// extremes (migration 0115). A band is also too weak (a dust wick can sit inside 2x) and too strong
// (it clips a genuine large move). Filter on trade size, never price divergence: a $100,000
// fat-finger is a real market event and must show. Zero Rat when no candidates (defensive).
func selectExtreme(cands []*big.Rat, isHigh bool) *big.Rat {
	if len(cands) == 0 {
		return new(big.Rat)
	}
	moreExtreme := func(a, b *big.Rat) bool { return a.Cmp(b) > 0 } // higher wins
	if !isHigh {
		moreExtreme = func(a, b *big.Rat) bool { return a.Cmp(b) < 0 } // lower wins
	}
	best := cands[0]
	for _, c := range cands[1:] {
		if moreExtreme(c, best) {
			best = c
		}
	}
	return best
}

// ratFromDecimal parses a NUMERIC decimal string (e.g. "0.19056",
// "3997807371333934") into a big.Rat. Returns nil on parse failure or
// empty input.
func ratFromDecimal(s string) *big.Rat {
	if s == "" {
		return nil
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil
	}
	return r
}

// seriesVolumeText is the one wire rendering of a series bar's v_base /
// v_quote: integer smallest-unit text. The true value is a sum of integer
// amounts; the CAGG's vwap*volume leg carries sub-unit NUMERIC noise on
// either side of it, so round to nearest (half up; volumes are >= 0).
func seriesVolumeText(r *big.Rat) string {
	if r == nil {
		return "0"
	}
	twice := new(big.Int).Mul(r.Num(), big.NewInt(2))
	twice.Add(twice, r.Denom())
	return twice.Quo(twice, new(big.Int).Mul(r.Denom(), big.NewInt(2))).String()
}

// normalizeOHLCSeriesVolumes re-renders pass-through CAGG rows through
// [seriesVolumeText] so the native path emits what finalize emits.
func normalizeOHLCSeriesVolumes(bars []OHLCSeriesBar) {
	for i := range bars {
		if v := ratFromDecimal(bars[i].VBase); v != nil {
			bars[i].VBase = seriesVolumeText(v)
		}
		if v := ratFromDecimal(bars[i].VQuote); v != nil {
			bars[i].VQuote = seriesVolumeText(v)
		}
	}
}
