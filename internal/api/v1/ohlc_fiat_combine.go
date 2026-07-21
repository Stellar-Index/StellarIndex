package v1

import (
	"context"
	"math/big"
	"sort"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
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
// consistent with the live /v1/price path (CLAUDE.md "stablecoins-as-fiat
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
	for _, sp := range src {
		bars, err := s.history.OHLCSeries(ctx, sp, string(interval), from, to, limit)
		if err != nil {
			// Propagate real errors (DB / context) — a constituent that
			// simply has no rows returns an empty slice + nil, not an error.
			return nil, err
		}
		for i := range bars {
			b := &bars[i]
			a := acc[b.T]
			if a == nil {
				a = newOHLCBucketAcc()
				acc[b.T] = a
			}
			a.add(b)
		}
	}
	if len(acc) == 0 {
		return nil, nil
	}

	out := make([]OHLCSeriesBar, 0, len(acc))
	for t, a := range acc {
		out = append(out, a.finalize(t))
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
	return out
}

// ohlcBucketAcc accumulates the combine across constituent bars sharing a
// bucket timestamp. All arithmetic is big.Rat/big.Int to preserve the
// NUMERIC precision the wire contract promises (no float round-trip).
type ohlcBucketAcc struct {
	openNum  *big.Rat   // Σ(open  · base_vol)
	closeNum *big.Rat   // Σ(close · base_vol)
	baseVol  *big.Rat   // Σ base_vol (weight denominator)
	quoteVol *big.Rat   // Σ quote_vol
	highs    []*big.Rat // per-constituent candidate highs (outliers dropped at finalize)
	lows     []*big.Rat // per-constituent candidate lows
	n        int64
}

func newOHLCBucketAcc() *ohlcBucketAcc {
	return &ohlcBucketAcc{
		openNum:  new(big.Rat),
		closeNum: new(big.Rat),
		baseVol:  new(big.Rat),
		quoteVol: new(big.Rat),
	}
}

func (a *ohlcBucketAcc) add(b *OHLCSeriesBar) {
	open := ratFromDecimal(b.O)
	closeP := ratFromDecimal(b.C)
	high := ratFromDecimal(b.H)
	low := ratFromDecimal(b.L)
	bv := ratFromDecimal(b.VBase)
	qv := ratFromDecimal(b.VQuote)
	if open == nil || closeP == nil || high == nil || low == nil || bv == nil {
		return // unparseable row — skip rather than corrupt the bucket
	}

	a.openNum.Add(a.openNum, new(big.Rat).Mul(open, bv))
	a.closeNum.Add(a.closeNum, new(big.Rat).Mul(closeP, bv))
	a.baseVol.Add(a.baseVol, bv)
	if qv != nil {
		a.quoteVol.Add(a.quoteVol, qv)
	}
	a.n += b.N
	// Collect the per-constituent extremes as candidates; finalize DROPS the
	// out-of-band ones (thin-book / fat-finger prints) against the bucket VWAP.
	a.highs = append(a.highs, high)
	a.lows = append(a.lows, low)
}

// combinedOutlierBandRatio bounds a combined bar's high/low against the
// bucket's volume-weighted price. The USD-peg expansion folds thin SDEX
// stablecoin books into the fiat series, and a single fat-finger print there
// (site audit S-012: one 1.00-USDC-per-XLM fill, 5.6× market; and the 2026-07
// XLM/USD >$0.50 wicks) would otherwise set the served high for the flagship
// pair. Prints beyond this band are DROPPED — not clamped to the ceiling, which
// still served a visible ~3× wick ($0.56 on a $0.187 XLM). 2× never clips a
// plausible intra-bucket move (largest observed XLM/USD 1h range is well under
// 2×) while removing dust wicks entirely. Applies ONLY to the synthetic
// combined series — direct venue series serve their true extremes untouched.
var combinedOutlierBandRatio = big.NewRat(2, 1)

func (a *ohlcBucketAcc) finalize(t time.Time) OHLCSeriesBar {
	open, closeP := new(big.Rat), new(big.Rat)
	if a.baseVol.Sign() > 0 {
		open.Quo(a.openNum, a.baseVol)
		closeP.Quo(a.closeNum, a.baseVol)
	}
	// Anchor the outlier band on the bucket VWAP (the high-volume CEX
	// constituents dominate it, so it's the real price even when a single
	// thin-book print skews one constituent's high/low).
	var vwap *big.Rat
	if a.baseVol.Sign() > 0 && a.quoteVol.Sign() > 0 {
		vwap = new(big.Rat).Quo(a.quoteVol, a.baseVol)
	}
	high := selectExtreme(a.highs, vwap, true)
	low := selectExtreme(a.lows, vwap, false)
	return OHLCSeriesBar{
		T:      t,
		O:      ratToDecimal(open, ohlcPriceDigits),
		H:      ratToDecimal(high, ohlcPriceDigits),
		L:      ratToDecimal(low, ohlcPriceDigits),
		C:      ratToDecimal(closeP, ohlcPriceDigits),
		VBase:  ratToDecimal(a.baseVol, 0),
		VQuote: ratToDecimal(a.quoteVol, ohlcPriceDigits),
		N:      a.n,
	}
}

// selectExtreme returns the bucket high (isHigh=true) or low from the candidate
// per-constituent extremes, DROPPING dust/fat-finger prints: a high above
// combinedOutlierBandRatio × vwap (or a low below vwap / band) is a thin-book
// artifact and is excluded, so the served extreme is the true market extreme
// among the in-band prints — not a synthetic clamp ceiling. Falls back to the
// least-outlier candidate when every print is out of band (pathological) or vwap
// is unavailable, and to a zero Rat when there are no candidates (defensive:
// finalize only runs with ≥1 bar).
func selectExtreme(cands []*big.Rat, vwap *big.Rat, isHigh bool) *big.Rat {
	if len(cands) == 0 {
		return new(big.Rat)
	}
	// Collapse the high/low direction into two comparators up front so the
	// scan below is direction-agnostic: `moreExtreme` picks the bucket
	// extreme, `inBand` decides whether a print survives the outlier filter.
	moreExtreme := func(a, b *big.Rat) bool { return a.Cmp(b) > 0 } // higher wins
	inBand := func(c, bound *big.Rat) bool { return c.Cmp(bound) <= 0 }
	if !isHigh {
		moreExtreme = func(a, b *big.Rat) bool { return a.Cmp(b) < 0 } // lower wins
		inBand = func(c, bound *big.Rat) bool { return c.Cmp(bound) >= 0 }
	}
	bound := outlierBound(vwap, isHigh)

	var best, fallback *big.Rat
	for _, c := range cands {
		// fallback = the LEAST extreme candidate (min high / max low) so an
		// all-outlier bucket still serves something bounded, not the wick.
		if fallback == nil || moreExtreme(fallback, c) {
			fallback = c
		}
		if bound != nil && !inBand(c, bound) {
			continue // dust / fat-finger print — drop it
		}
		if best == nil || moreExtreme(c, best) {
			best = c
		}
	}
	if best != nil {
		return best
	}
	return fallback
}

// outlierBound returns the in-band ceiling (highs) or floor (lows) around the
// bucket VWAP, or nil when VWAP is unavailable — in which case nothing is
// dropped and the raw extreme stands.
func outlierBound(vwap *big.Rat, isHigh bool) *big.Rat {
	if vwap == nil {
		return nil
	}
	if isHigh {
		return new(big.Rat).Mul(vwap, combinedOutlierBandRatio)
	}
	return new(big.Rat).Quo(vwap, combinedOutlierBandRatio)
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
