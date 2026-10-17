package orchestrator

import (
	"context"
	"math/big"
	"sort"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// fetchTradesDetectTruncation wraps the store fetch with the per-query cap. A capped leg's older minutes are filled from minute sums
// ([Orchestrator.fillFromMinuteSums]); what stays uncovered is warned, counted and returned as coveredFrom (zero when whole).
// `target` is the aggregation target, `fetch` the pair actually queried (a stablecoin-backer pair under proxy expansion). weights maps a
// minute row's Trade.ID() to the trades it stands for.
func (o *Orchestrator) fetchTradesDetectTruncation(
	ctx context.Context, target, fetch canonical.Pair, from, to time.Time,
) (trades []canonical.Trade, weights map[string]int64, coveredFrom time.Time, err error) {
	t, err := o.store.TradesInRange(ctx, fetch, from, to, o.cfg.MaxTradesPerWindow)
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	if len(t) == 0 || len(t) < o.cfg.MaxTradesPerWindow {
		return t, nil, time.Time{}, nil
	}
	coveredFrom = t[0].Timestamp
	for i := range t {
		if t[i].Timestamp.Before(coveredFrom) {
			coveredFrom = t[i].Timestamp
		}
	}
	if filled, w, cf, ok := o.fillFromMinuteSums(ctx, fetch, from, to, t, coveredFrom); ok {
		t, weights, coveredFrom = filled, w, cf
	}
	if !coveredFrom.IsZero() {
		obs.AggregatorWindowTruncatedTotal.Inc()
		o.logger.Warn("trade window truncated at MaxTradesPerWindow — VWAP over the newest slice only",
			"target", target.String(),
			"fetch_pair", fetch.String(),
			"cap", o.cfg.MaxTradesPerWindow,
			"from", from.UTC(),
			"to", to.UTC(),
			"covered_from", coveredFrom.UTC(),
		)
	}
	return t, weights, coveredFrom, nil
}

// TradeSumsStore is the optional minute-sums read a Store may also implement; without it a capped leg stays truncated.
type TradeSumsStore interface {
	TradeSums1mInRange(ctx context.Context, p canonical.Pair, from, to time.Time) ([]timescale.TradeMinuteSum, time.Time, error)
}

// tradeSumsHorizon is the longest window minute sums stand in for: trade_sums_1m re-materialises late inserts 26 hours back (0217).
const tradeSumsHorizon = 25 * time.Hour

// minuteRawBudget caps the over-spread minutes one capped leg re-reads raw before it stays truncated from the newest unresolved one.
const minuteRawBudget = 10

// minuteSumTxPrefix marks a minute row's TxHash; with the fetched pair it keeps Trade.ID() unique per (source, leg, minute).
const minuteSumTxPrefix = "sum1m:"

// fillFromMinuteSums replaces a capped read's older part with one row per (source, minute) from trade_sums_1m. Raw rows are kept from
// the minute after coveredFrom's, since the cap can cut inside a minute or a timestamp. A minute whose prints spread wider than the
// outlier band is re-read raw (an off-price print must not hide inside honest volume); one that cannot be resolved ends coverage there.
// ok=false leaves the raw read as is: no sums store, a window past the horizon or not minute-aligned, a read error, or an unmaterialised or invalidated range.
func (o *Orchestrator) fillFromMinuteSums(
	ctx context.Context, fetch canonical.Pair, from, to time.Time, raw []canonical.Trade, coveredFrom time.Time,
) (trades []canonical.Trade, weights map[string]int64, newCoveredFrom time.Time, ok bool) {
	ss, isSums := o.store.(TradeSumsStore)
	if !isSums || to.Sub(from) > tradeSumsHorizon || !from.Equal(from.Truncate(time.Minute)) {
		return nil, nil, time.Time{}, false
	}
	m := coveredFrom.Truncate(time.Minute).Add(time.Minute)
	if m.After(to) {
		return nil, nil, time.Time{}, false
	}
	sums, watermark, err := ss.TradeSums1mInRange(ctx, fetch, from, m)
	if err != nil {
		o.logger.Warn("minute-sums read failed; leg stays truncated", "fetch_pair", fetch.String(), "err", err)
		return nil, nil, time.Time{}, false
	}
	if watermark.Before(m) {
		return nil, nil, time.Time{}, false
	}
	rows := foldMinuteSums(sums)
	cut, extra := o.resolveWideMinutes(ctx, fetch, rows)

	weights = make(map[string]int64, len(rows))
	trades = make([]canonical.Trade, 0, len(rows)+len(extra)+len(raw))
	for _, r := range rows {
		if r.bucket.Before(cut) || r.wide {
			continue
		}
		t := canonical.Trade{
			Source:      r.source,
			TxHash:      minuteSumTxPrefix + fetch.String(),
			Timestamp:   r.bucket,
			Pair:        fetch,
			BaseAmount:  canonical.NewAmount(r.base),
			QuoteAmount: canonical.NewAmount(r.quote),
		}
		weights[t.ID()] = r.count
		trades = append(trades, t)
	}
	for i := range extra {
		if !extra[i].Timestamp.Before(cut) {
			trades = append(trades, extra[i])
		}
	}
	for i := range raw {
		if !raw[i].Timestamp.Before(m) {
			trades = append(trades, raw[i])
		}
	}
	sort.SliceStable(trades, func(i, j int) bool { return trades[i].Timestamp.Before(trades[j].Timestamp) })
	return trades, weights, cut, true
}

// minuteRow is one (source, minute) of a leg, both stored directions folded together.
type minuteRow struct {
	source      string
	bucket      time.Time
	base, quote *big.Int
	count       int64
	lo, hi      *big.Rat
	wide        bool
}

// foldMinuteSums merges a leg's two stored directions into one row per (source, minute), oldest first.
func foldMinuteSums(sums []timescale.TradeMinuteSum) []*minuteRow {
	idx := make(map[string]*minuteRow, len(sums))
	out := make([]*minuteRow, 0, len(sums))
	for i := range sums {
		s := &sums[i]
		key := s.Source + "|" + s.Bucket.UTC().Format(time.RFC3339)
		r, seen := idx[key]
		if !seen {
			r = &minuteRow{
				source: s.Source, bucket: s.Bucket.UTC(), base: new(big.Int), quote: new(big.Int),
				lo: new(big.Rat).Set(s.MinPrice), hi: new(big.Rat).Set(s.MaxPrice),
			}
			idx[key] = r
			out = append(out, r)
		}
		r.base.Add(r.base, s.BaseSum.BigInt())
		r.quote.Add(r.quote, s.QuoteSum.BigInt())
		r.count += s.Count
		if s.MinPrice.Cmp(r.lo) < 0 {
			r.lo.Set(s.MinPrice)
		}
		if s.MaxPrice.Cmp(r.hi) > 0 {
			r.hi.Set(s.MaxPrice)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].bucket.Before(out[j].bucket) })
	return out
}

// resolveWideMinutes marks rows whose spread exceeds the outlier band and re-reads those minutes raw, newest first, within
// [minuteRawBudget]. It returns the raw rows read and cut, the start of coverage: the zero time when every wide minute was resolved,
// else the end of the newest one that was not.
func (o *Orchestrator) resolveWideMinutes(ctx context.Context, fetch canonical.Pair, rows []*minuteRow) (cut time.Time, extra []canonical.Trade) {
	band := o.minuteSpreadBand()
	if band == nil {
		return time.Time{}, nil
	}
	wide := map[time.Time]bool{}
	for _, r := range rows {
		if o.votesInVWAP(r.source) && spreadExceeds(r.lo, r.hi, band) {
			wide[r.bucket] = true
		}
	}
	// The raw re-read covers every source of the minute, so all its rows give way.
	for _, r := range rows {
		r.wide = wide[r.bucket]
	}
	buckets := make([]time.Time, 0, len(wide))
	for b := range wide {
		buckets = append(buckets, b)
	}
	sort.Slice(buckets, func(i, j int) bool { return buckets[i].After(buckets[j]) })
	for i, b := range buckets {
		if i >= minuteRawBudget {
			return b.Add(time.Minute), extra
		}
		t, err := o.store.TradesInRange(ctx, fetch, b, b.Add(time.Minute), o.cfg.MaxTradesPerWindow)
		if err != nil || len(t) >= o.cfg.MaxTradesPerWindow {
			return b.Add(time.Minute), extra
		}
		extra = append(extra, t...)
	}
	return time.Time{}, extra
}

// minuteSpreadBand is the largest r·(r−1), r = max/min price, at which every print of a minute scores within sigma of any centre inside
// [min, max] against the local scale floor (aggregate's 0.25%): sigma/400. nil when the outlier filter is off.
func (o *Orchestrator) minuteSpreadBand() *big.Rat {
	if o.cfg.OutlierSigmaThreshold <= 0 {
		return nil
	}
	sigma := new(big.Rat).SetFloat64(o.cfg.OutlierSigmaThreshold)
	if sigma == nil {
		return nil
	}
	return sigma.Quo(sigma, big.NewRat(400, 1))
}

func spreadExceeds(lo, hi, band *big.Rat) bool {
	r := new(big.Rat).Quo(hi, lo)
	rm1 := new(big.Rat).Sub(r, big.NewRat(1, 1))
	return r.Mul(r, rm1).Cmp(band) > 0
}

// votesInVWAP reports whether decideBucket keeps a source's trades past its source filters; a wide minute of a dropped source costs no re-read.
func (o *Orchestrator) votesInVWAP(source string) bool {
	for _, s := range o.cfg.ExcludedSources {
		if s == source {
			return false
		}
	}
	if o.cfg.DisableClassFilter {
		return true
	}
	md := external.Lookup(source)
	return md.Class == external.ClassExchange && md.IncludeInVWAP
}

// widenCoverage folds one source pair's capped-read point into c.
func widenCoverage(c cachekeys.WindowCoverage, coveredFrom time.Time) cachekeys.WindowCoverage {
	if coveredFrom.IsZero() {
		return c
	}
	if !c.Truncated || coveredFrom.After(c.CoveredFrom) {
		return cachekeys.WindowCoverage{Truncated: true, CoveredFrom: coveredFrom.UTC()}
	}
	return c
}
