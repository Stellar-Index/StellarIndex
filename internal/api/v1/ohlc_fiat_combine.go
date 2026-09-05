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

// ohlcSeriesFiatCombined builds a fiat-denominated (e.g. XLM/USD) OHLC
// series by COMBINING every USD-pegged constituent series per bucket,
// rather than the first-hit single-pair read used for non-fiat quotes.
//
// Why: the continuous aggregates key bars by the real stored quote_asset
// (`native/USDC-GA5Z…`, `crypto:XLM/fiat:USD`, …). A fiat quote like
// `fiat:USD` has no trades of its own except the recent direct CEX feeds,
// so first-hit served only ~5 weeks while the deep history sits under the
// USD-pegged stablecoin pairs (Circle USDC back to 2021). Combining them
// — using the SAME constituent set the live aggregator computes its VWAP
// over (aggregate.ExpandTargetPairWithClassicPegs) — yields the full
// multi-year series and keeps the historical bars methodologically
// consistent with the live /v1/price path (AGENTS.md "stablecoins-as-fiat
// is aggregator policy, late-bound at compute time").
//
// Combine math per bucket (exact in NUMERIC/big.Rat):
//   - volume      = Σ base_volume                (exact)
//   - quote_vol   = Σ quote_volume               (exact)
//   - high / low  = max(high) / min(low)         (exact)
//   - open/close  = Σ(price·base_vol) / Σ base_vol  (base-volume-weighted)
//     — the only approximation, and only in buckets where >1 constituent
//     trades; deep-history buckets have a single constituent (USDC) so
//     open/close are exact there. Base-volume weighting matches the VWAP
//     definition (Σ price·base / Σ base).
//
// SCALE (CS-040 money-path, series arm). Every one of those sums is over
// SMALLEST-UNIT amounts, and the smallest unit is a per-SOURCE scale: an
// on-chain DEX leg is 7-decimal stroops, a CEX leg 8, an FX poller 6. The
// constituent set really does span them — on r1 today `native/fiat:USD`
// combines `native/<USDC classic>` (sdex, 7dp) with
// `crypto:XLM/crypto:USDT` (binance, 8dp) and `crypto:XLM/fiat:USD`
// (bitstamp/coinbase/kraken, 8dp) in the SAME buckets — so summing the
// raw integers adds incommensurable quantities and weights the 8dp legs
// 10x per decimal against the 7dp one. Measured on the 2026-09-05 02:00Z
// 1h bar: the sdex leg's 135,713.79 XLM entered the combine as 13,571.38,
// the served v_base understated the market by 3.41%, and that leg carried
// 0.39% of the open/close weight where it should carry 3.79%.
//
// So every bar is lifted to one common scale before it is accumulated —
// the MAXIMUM scale present in the response, so each lift is an exact
// integer multiply by 10^(max−scale) ≥ 1 with no division and no
// precision loss (ADR-0003). This is the bar-level twin of
// [aggregate.NormalizeAmountScale], which [Server.fiatCombinedTrades]
// already applies to the raw-trade POINT path over the identical
// constituent set; the two now agree by construction rather than by the
// accident of every fixture being single-scale. A response whose bars all
// share one scale — every non-fiat quote, and any fiat window served by
// one venue class — is byte-identical to before, because every factor is
// 10^0 = 1.
//
// A bar states its own scale rather than having one guessed from its pair
// spelling: [OHLCSeriesBar.Sources] carries the CAGG's own
// `array_agg(DISTINCT source)` column. Spelling is not scale — the
// registry itself records that on-chain venues stamp at the ASSET decimals
// — so inferring 7 from a classic quote id would be a guess that happens
// to hold today.
//
// Each constituent read goes through the cached HistoryReader, so repeat
// requests hit the per-pair cache.
func (s *Server) ohlcSeriesFiatCombined(
	ctx context.Context,
	pair canonical.Pair,
	interval ohlcInterval,
	from, to time.Time,
	limit int,
) ([]OHLCSeriesBar, error) {
	src := s.usdPeggedConstituents(pair)

	acc := make(map[time.Time]*ohlcBucketAcc, 256)
	// commonScale is the finest smallest-unit scale any bar in this
	// response declares; every bucket is lifted to it in finalize. It
	// spans the whole response rather than one bucket, so v_base never
	// changes units part-way down a series.
	commonScale := ohlcBarScaleUnknown
	scales := make(map[int]struct{}, 4)
	for _, sp := range src {
		bars, err := s.history.OHLCSeries(ctx, sp, string(interval), from, to, limit)
		if err != nil {
			// Propagate real errors (DB / context) — a constituent that
			// simply has no rows returns an empty slice + nil, not an error.
			return nil, err
		}
		for i := range bars {
			b := &bars[i]
			scale := barScaleDecimals(b.Sources)
			if scale > commonScale {
				commonScale = scale
			}
			scales[scale] = struct{}{}
			a := acc[b.T]
			if a == nil {
				a = newOHLCBucketAcc()
				acc[b.T] = a
			}
			a.add(b, scale)
		}
	}
	if len(acc) == 0 {
		return nil, nil
	}

	factors := make(map[int]*big.Rat, len(scales))
	for sc := range scales {
		factors[sc] = ohlcScaleFactor(sc, commonScale)
	}

	out := make([]OHLCSeriesBar, 0, len(acc))
	for t, a := range acc {
		out = append(out, a.finalize(t, factors))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].T.Before(out[j].T) })
	// Match OHLCSeries' earliest-N-in-window semantics (ORDER BY bucket
	// ASC LIMIT n): the handler sizes [from,to] to `limit` intervals, so
	// this only bites when a caller passes an explicit wide window.
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// usdPeggedConstituents enumerates the source (base,quote) cagg pairs to
// combine for a fiat-quoted target, across both XLM dual-form base
// aliases (native ↔ crypto:XLM) and the aggregator's USD-peg expansion
// (direct pair + stablecoin backers + operator-declared classic pegs).
// Deduplicated.
func (s *Server) usdPeggedConstituents(pair canonical.Pair) []canonical.Pair {
	seen := make(map[string]struct{})
	var out []canonical.Pair
	for _, b := range assetAliases(pair.Base) {
		tgt, err := canonical.NewPair(b, pair.Quote)
		if err != nil {
			continue
		}
		expanded, err := aggregate.ExpandTargetPairWithClassicPegs(tgt, s.usdPeggedClassics)
		if err != nil {
			// Malformed target — fall back to the direct pair only.
			expanded = []canonical.Pair{tgt}
		}
		for _, sp := range expanded {
			k := sp.Base.String() + "\x00" + sp.Quote.String()
			if _, dup := seen[k]; dup {
				continue
			}
			seen[k] = struct{}{}
			out = append(out, sp)
		}
	}
	// The `seen` key above is the ORDERED pair, which was the whole
	// market back when a read bound one orientation. Both readers this
	// set feeds — Store.TradesInRange for the point path and
	// Store.OHLCSeries for the series — now span both stored
	// directions, so a constituent and its flip would be read twice and
	// merged into one bucket. Deduplicating by MARKET keeps the probe
	// ([Server.ohlcCoverageSet]) spanning exactly what the combine
	// reads, because both derive from this one list.
	return distinctMarkets(out)
}

// fiatCombinedTrades is the POINT-path twin of
// [Server.ohlcSeriesFiatCombined]: it reads the raw trades of EVERY
// constituent of a fiat-quoted target — the SAME set
// [Server.usdPeggedConstituents] feeds the series combine — and merges them
// into one chronologically-ordered population. Returns
// (trades, proxied, err); `proxied` drives flags.triangulated.
//
// C1-024 (audit-2026-07-23): the point path used to read the LITERAL pair
// and, only when that came back empty, retry each operator-declared classic
// peg and take the FIRST non-empty one. So exactly one constituent ever
// backed a point quote, while the series combined all of them — and the
// point path's fallback set was structurally narrower besides: no base
// aliases (the CEX `crypto:XLM/fiat:USD` stream was unreachable from
// `?base=native`), no `crypto:USDC`/`crypto:USDT`/… backers, and hard-gated
// on quote.Code == "USD" so a fiat:EUR point 404'd against a populated EUR
// series. `/v1/vwap?base=native&quote=fiat:USD` and
// `/v1/ohlc?interval=1h&base=native&quote=fiat:USD` — the same question
// asked two ways — therefore answered from different trade populations, and
// a point quote could not be reconciled against the series it belongs to.
// The series methodology is the authority (it is the live aggregator's own
// source set, aggregate.ExpandTargetPairWithClassicPegs — see
// [Server.ohlcSeriesFiatCombined]); the point path now derives from the
// identical constituent selection, so point == series at shared timestamps.
//
// Constituent read errors PROPAGATE rather than being skipped: dropping a
// constituent would silently narrow the methodology back to the divergence
// this fix removes, and a quietly-narrower money answer is worse than a 500.
// This matches [Server.ohlcSeriesFiatCombined], which also propagates.
//
// Each constituent is fetched with the caller's `maxTrades` cap (newest-N
// per pair, per the reader's `ts DESC` LIMIT — F-1319) and the merged set is
// then trimmed to the newest `maxTrades` overall, preserving the callers'
// `len(trades) == maxTrades` truncation signal.
func (s *Server) fiatCombinedTrades(
	ctx context.Context, pair canonical.Pair, from, to time.Time, maxTrades int,
) ([]canonical.Trade, bool, error) {
	var merged []canonical.Trade
	proxied := false
	for _, sp := range s.usdPeggedConstituents(pair) {
		batch, err := s.history.TradesInRange(ctx, sp, from, to, maxTrades)
		if err != nil {
			return nil, false, err
		}
		if len(batch) == 0 {
			continue
		}
		// Triangulated means the QUOTE leg was proxied (X/fiat:USD served
		// from X/USDC), which is what the flag has always meant on this
		// path. A base-alias constituent (`crypto:XLM/fiat:USD` for a
		// `?base=native` query) is the same asset in another canonical
		// form, not a proxy, so it does not raise the flag.
		if !sp.Quote.Equal(pair.Quote) {
			proxied = true
		}
		merged = append(merged, batch...)
	}
	sortTradesChronological(merged)
	if maxTrades > 0 && len(merged) > maxTrades {
		merged = merged[len(merged)-maxTrades:]
	}
	// Scale-normalize before the callers (handleVWAP / handleTWAP /
	// single-bar handleOHLC) feed this cross-source slice to
	// aggregate.VWAP / aggregate.ComputeOHLC. usdPeggedConstituents merges
	// on-chain legs (native/USDC-classic, 7dp) with CEX legs
	// (crypto:XLM/crypto:USDT, 8dp) into one population, so without lifting
	// every trade to a common smallest-unit scale the Σquote/Σbase weighted
	// mean would over-weight the finer-scaled source ~10× per decimal of
	// scale difference (CS-040 money-path). Uniform windows — all-CEX or
	// all-on-chain, the common case — are returned byte-identical, so only
	// genuinely mixed windows move, toward the true real-volume-weighted
	// price. TWAP is time-weighted and thus unaffected either way (the
	// per-trade ratio it weights is scale-invariant); normalizing is a
	// no-op there, applied uniformly to keep one merge path.
	merged = aggregate.NormalizeAmountScale(merged, amountScaleDecimalsFor)
	return merged, proxied, nil
}

// amountScaleDecimalsFor resolves a trade source's smallest-unit scale from
// the external registry (8 for CEX/aggregator, 7 for on-chain DEX, 6 for the
// FX pollers — CS-040). Extracted as a package-level func so
// aggregate.NormalizeAmountScale can stay free of an internal/sources/external
// import (that package imports aggregate — the reverse edge is a cycle).
func amountScaleDecimalsFor(source string) int {
	return external.Lookup(source).AmountScaleDecimals()
}

// sortTradesChronological orders a merged multi-constituent trade set by
// close time ascending. Required: [aggregate.ComputeOHLC] and
// [aggregate.TWAP] derive open/close and the time weights from slice order
// and deliberately do not sort internally.
//
// The tiebreak is the trades hypertable's primary key (ledger, source,
// tx_hash, op_index), NOT slice position. [Server.usdPeggedConstituents]'
// order is map-iteration-dependent (aggregate.FiatBackers ranges a map), so
// a merge-order-preserving sort would let two regions — or two consecutive
// requests in one process — order same-timestamp prints differently and
// serve different open/close for the identical window, breaking ADR-0015's
// "all regions return the same rate".
func sortTradesChronological(trades []canonical.Trade) {
	sort.Slice(trades, func(i, j int) bool {
		a, b := &trades[i], &trades[j]
		if !a.Timestamp.Equal(b.Timestamp) {
			return a.Timestamp.Before(b.Timestamp)
		}
		if a.Ledger != b.Ledger {
			return a.Ledger < b.Ledger
		}
		if a.Source != b.Source {
			return a.Source < b.Source
		}
		if a.TxHash != b.TxHash {
			return a.TxHash < b.TxHash
		}
		return a.OpIndex < b.OpIndex
	})
}

// ohlcBarScaleUnknown is the scale of a bar whose reader reported no
// contributing sources. It is a distinct value rather than a plausible
// default: a bar of unknown scale is one that CANNOT be lifted, and
// silently calling it 8 (the registry fallback for an unrecognised
// source) would re-introduce exactly the 10x mis-weight this file
// corrects. It never survives a production read — the CAGG populates
// `sources` for every materialised bucket — and it lifts by a factor of
// 1, which is the pre-fix behaviour and so can only ever leave a bar
// where it already was.
const ohlcBarScaleUnknown = -1

// barScaleDecimals resolves a combined bar's smallest-unit scale from
// the venues that contributed to it, via the SAME resolver the raw-trade
// point path hands to [aggregate.NormalizeAmountScale]. Sharing the
// resolver is what keeps point and series on one answer (C1-024).
//
// The MAXIMUM across the bar is taken. Every bar on r1 is homogeneous —
// 129,854 distinct pair spellings over 400 days of prices_1d, not one of
// them written at two scales, checked 2026-09-05 — so max is that single
// scale and the lift is exact. If a bar ever does mix scales internally
// its stored volume is already a sum of incommensurable integers that no
// read-time factor can repair, and max is the choice that gives such a
// bar the SMALLEST lift, so it can never inflate its own weight against
// its peers. That is the same direction launch-plan row 1.15 requires of
// a thin venue beside book data.
func barScaleDecimals(sources []string) int {
	scale := ohlcBarScaleUnknown
	for _, src := range sources {
		if d := amountScaleDecimalsFor(src); d > scale {
			scale = d
		}
	}
	return scale
}

// ohlcScaleFactor is the exact integer multiplier that lifts a bar at
// `scale` to `common`. Mirrors [aggregate.NormalizeAmountScale]: the
// common scale is the maximum in the set, so the exponent is never
// negative and nothing is ever divided. Returns 1 for a bar already at
// the common scale and for one whose scale is unknown.
func ohlcScaleFactor(scale, common int) *big.Rat {
	if scale == ohlcBarScaleUnknown || common == ohlcBarScaleUnknown || common <= scale {
		return new(big.Rat).SetInt64(1)
	}
	exp := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(common-scale)), nil)
	return new(big.Rat).SetInt(exp)
}

// ohlcBucketAcc accumulates the combine across constituent bars sharing a
// bucket timestamp. All arithmetic is big.Rat/big.Int to preserve the
// NUMERIC precision the wire contract promises (no float round-trip).
//
// The volume sums are kept PER SCALE rather than as one running total,
// because the scale they must all be lifted to is the maximum across the
// whole response and is not known until every constituent has been read.
// Prices are scale-invariant (a ratio of two legs a source stamps at one
// scale), so the extremes and the trade count need no such split.
type ohlcBucketAcc struct {
	byScale map[int]*ohlcScaleAcc // keyed by the contributing bars' scale
	highs   []*big.Rat            // per-constituent highs (bucket high = max)
	lows    []*big.Rat            // per-constituent lows  (bucket low  = min)
	n       int64
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
	return &ohlcBucketAcc{byScale: make(map[int]*ohlcScaleAcc, 2)}
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
	// Collect the per-constituent extremes; the bucket extreme is simply the
	// max/min across them (each constituent's own extreme already excludes
	// dust — see selectExtreme).
	a.highs = append(a.highs, high)
	a.lows = append(a.lows, low)
}

// finalize renders the bucket, lifting each scale's totals by its factor
// from `factors` (built once per response by the caller) so the served
// volumes and the volume-weighted open/close are all in one unit.
//
// The per-scale totals are summed in map-iteration order, which is not
// deterministic. That is safe HERE and only here: these are exact
// big.Rat additions, and rational addition is associative and
// commutative exactly, so the sum is identical whatever order it runs in
// — unlike the point path, where slice order picks open/close and
// [sortTradesChronological] therefore has to impose a total order for
// ADR-0015.
func (a *ohlcBucketAcc) finalize(t time.Time, factors map[int]*big.Rat) OHLCSeriesBar {
	openNum, closeNum := new(big.Rat), new(big.Rat)
	baseVol, quoteVol := new(big.Rat), new(big.Rat)
	for scale, sa := range a.byScale {
		f, ok := factors[scale]
		if !ok {
			f = new(big.Rat).SetInt64(1)
		}
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
	return OHLCSeriesBar{
		T:      t,
		O:      ratToDecimal(open, ohlcPriceDigits),
		H:      ratToDecimal(high, ohlcPriceDigits),
		L:      ratToDecimal(low, ohlcPriceDigits),
		C:      ratToDecimal(closeP, ohlcPriceDigits),
		VBase:  ratToDecimal(baseVol, 0),
		VQuote: ratToDecimal(quoteVol, ohlcPriceDigits),
		N:      a.n,
	}
}

// selectExtreme returns the bucket high (isHigh=true) or low across the
// per-constituent extremes: the plain max / min, with NO price-distance
// filtering.
//
// It used to drop candidates outside `combinedOutlierBandRatio` (2×) of the
// bucket VWAP. That band is GONE (audit B11-F1; operator decision 2026-07-22 in
// docs/operations/finding-dust-trades-set-chart-extremes.md): every wick it was
// built for was DUST, and dust is now excluded upstream by the $0.01 notional
// floor on the CAGG extremes (migration 0115) — at the individual-trade level
// the band could never reach. The band was also both too weak and too strong: it
// missed the 2026-07-17 XLM/USD 0.1333 wick (0.73× VWAP, comfortably in band)
// while being able to clip a genuine large move. Filter on trade SIZE, never on
// price divergence: a $100,000 fat-finger is a real market event and must show.
//
// Returns a zero Rat when there are no candidates (defensive: finalize only
// runs with ≥1 bar).
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
