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

// ohlcSeriesFiatCombined builds a fiat-quoted OHLC series by combining every USD-pegged constituent per bucket (fiat:USD itself
// has only recent CEX trades), using the aggregator's own set to stay consistent with /v1/price. Combining is exact in big.Rat,
// each bar lifted to the max scale first (ADR-0003). Held-back constituents (a declared peg's SAC wrapper) fill only buckets no
// established spelling answered: a thin Soroban print can move a book-backed high (docs/architecture/aggregation-plan.md, section
// 7.5). The bool is `proxied`, driving `flags.triangulated`.
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
	// Snapshot the answered buckets BEFORE the held-back pass so two held-back constituents
	// sharing an unanswered bucket still merge with each other.
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

// newestCombinedBars keeps the NEWEST `limit` of an ascending merged series. Each constituent read is capped to its own newest
// `limit` buckets, so the old end holds buckets a dense constituent was never asked for; the newest `limit` are safe by counting,
// and the same count covers the held-back gate.
func newestCombinedBars(out []OHLCSeriesBar, limit int) []OHLCSeriesBar {
	if limit > 0 && len(out) > limit {
		return out[len(out)-limit:]
	}
	return out
}

// usdPeggedConstituents concatenates [Server.usdPeggedConstituentSets], established first; the coverage probe ([Server.ohlcCoverageSet])
// measures it because an empty answer means BOTH sets were read.
func (s *Server) usdPeggedConstituents(pair canonical.Pair) []canonical.Pair {
	established, heldBack := s.usdPeggedConstituentSets(pair)
	out := make([]canonical.Pair, 0, len(established)+len(heldBack))
	out = append(out, established...)
	return append(out, heldBack...)
}

// usdPeggedConstituentSets partitions a fiat-quoted target's cagg pairs into `established` (the aggregator's set: XLM dual-form
// aliases crossed with USD pegs) and `heldBack` (a declared peg's SAC wrapper, since Soroban AMMs trade the wrapper), read only
// where established left a bucket unanswered. Classic-then-SAC across ALL peg families: a pool is far thinner than the book.
// Deduplicated by market; a held-back pair whose flip is established is dropped.
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

// dropMarketsIn removes from `pairs` every market already in `kept`, in either direction, so a
// market reachable under both sets belongs to the one read first.
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

// fiatCombinedTrades is the point-path twin of [Server.ohlcSeriesFiatCombined]: it merges the raw trades of EVERY constituent
// into one chronological population at one amount scale, so /v1/vwap and /v1/ohlc with quote=fiat:USD agree; `proxied` drives
// flags.triangulated. The gate uses [fiatPointGateInterval]. Read errors propagate (a quietly narrower money answer is worse than
// a 500); each read uses the caller's `maxTrades` and the merge is trimmed to the newest `maxTrades`, preserving the callers'
// `len(trades) == maxTrades` truncation signal.
func (s *Server) fiatCombinedTrades(
	ctx context.Context, pair canonical.Pair, from, to time.Time, maxTrades int,
) (aggregate.ScaledWindow, bool, error) {
	established, heldBack := s.usdPeggedConstituentSets(pair)
	merged, proxied, err := s.mergeConstituentTrades(ctx, established, pair, from, to, maxTrades)
	if err != nil {
		return aggregate.ScaledWindow{}, false, err
	}
	// Held-back spellings answer only the sub-windows the established ones left empty. Gating on the
	// whole window would make the point window its own bucket and diverge from the series.
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

// mergeConstituentTrades reads one constituent set into a single population and reports whether any quote leg proxied the requested one.
// Read errors propagate, as in [Server.fiatCombinedTrades].
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

// dropTradesInAnsweredBuckets removes trades whose gate bucket an established spelling already answered; with nothing answered (a
// pool- only asset) the input is returned untouched, since the alternative is no price at all.
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

// fiatPointGateBucket floors a trade's timestamp onto the gate grid. UTC first: buckets are
// compared as values, and identical instants carrying different locations are not equal.
func fiatPointGateBucket(ts time.Time) time.Time {
	return ts.UTC().Truncate(fiatPointGateInterval.duration())
}

// amountScaleDecimalsFor resolves a source's smallest-unit scale from the external registry (8 CEX, 7 on-chain DEX, 6 FX); a
// package- level func so aggregate.NormalizeAmountScale avoids importing external, which imports aggregate (a cycle).
func amountScaleDecimalsFor(source string) int {
	return external.Lookup(source).AmountScaleDecimals()
}

// sortTradesChronological orders merged trades by close time ascending, as [aggregate.ComputeOHLC] and [aggregate.TWAP] require;
// ties break by [aggregate.TradeOrderLess], not slice position, so served open/close never depends on merge order (ADR-0015).
func sortTradesChronological(trades []canonical.Trade) {
	sort.Slice(trades, func(i, j int) bool {
		return aggregate.TradeOrderLess(&trades[i], &trades[j])
	})
}

// ohlcBarScaleUnknown is the scale of a bar whose reader reported no sources: distinct from 8 (the registry fallback), which
// would reintroduce the 10x mis-weight this file corrects. It lifts by a factor of 1.
const ohlcBarScaleUnknown = -1

// barScaleDecimals resolves a combined bar's smallest-unit scale from its venues, as the point path does for
// [aggregate.NormalizeAmountScale];
// the maximum is taken (stored bars are homogeneous, so the lift is exact). Unknown when any source lacks an [external.Registry] entry:
// [external.Lookup] would answer the 8-decimal CEX default and misstate an unregistered on-chain DEX tenfold. Twin of
// [commonAmountScaleDecimals].
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

// fiatCombine is the running state of one fiat-combined response: one accumulator per bucket, each with its own scale. The lift target is
// per bucket, not per response, or the same day would render in different units by window width.
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

// combineConstituentBars reads each pair's bars and folds them into `c`; a non-nil `admit` decides per bucket whether a bar may
// contribute (confining the held-back pass to unanswered buckets). Read errors propagate (a narrower money answer is worse than a
// 500). Returns `proxied`.
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

// ohlcBucketAcc accumulates constituent bars sharing a bucket timestamp in big.Rat/big.Int (no float round-trip). Volume sums are
// kept per scale because the lift target (the bucket's maximum scale) is unknown until every constituent is read.
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

// ohlcScaleAcc is one bucket's running totals for bars at a single smallest-unit scale.
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
	// The bucket extreme is the max/min across per-constituent extremes (see selectExtreme).
	a.highs = append(a.highs, high)
	a.lows = append(a.lows, low)
}

// finalize renders the bucket, lifting each scale's totals to the bucket's common scale (stated as v_base_decimals/v_quote_decimals).
// Map-order summing is safe only because exact big.Rat addition is order-independent; the point path's open/close needs
// [sortTradesChronological] (ADR-0015).
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
	// A bucket holding an unknown-scale bar sums integers at no single scale, so it states none.
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
		// Every scale present was lifted to the bucket's common scale, so stating anything else
		// would mismatch the served integers.
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

// selectExtreme returns the bucket high (isHigh) or low across per-constituent extremes: plain max/min, with no price-distance filtering.
// Filter on trade size, never price divergence: a real $100,000 fat-finger must show (docs/operations/finding-dust-trades-set-chart-
// extremes.md).
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

// ratFromDecimal parses a NUMERIC decimal string into a big.Rat; nil on empty or bad input.
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

// seriesVolumeText is the one wire rendering of a series bar's v_base / v_quote: integer smallest-unit text, rounded half up
// because the CAGG's vwap*volume leg carries sub-unit NUMERIC noise.
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
