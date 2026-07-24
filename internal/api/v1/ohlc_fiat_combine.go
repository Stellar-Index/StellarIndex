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
	highs    []*big.Rat // per-constituent highs (bucket high = max)
	lows     []*big.Rat // per-constituent lows  (bucket low  = min)
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
	// Collect the per-constituent extremes; the bucket extreme is simply the
	// max/min across them (each constituent's own extreme already excludes
	// dust — see selectExtreme).
	a.highs = append(a.highs, high)
	a.lows = append(a.lows, low)
}

func (a *ohlcBucketAcc) finalize(t time.Time) OHLCSeriesBar {
	open, closeP := new(big.Rat), new(big.Rat)
	if a.baseVol.Sign() > 0 {
		open.Quo(a.openNum, a.baseVol)
		closeP.Quo(a.closeNum, a.baseVol)
	}
	high := selectExtreme(a.highs, true)
	low := selectExtreme(a.lows, false)
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
