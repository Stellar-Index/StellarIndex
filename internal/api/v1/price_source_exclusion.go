package v1

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// PriceExclusionGate is the operator source-exclusion seam for values the server computes or reads itself.
type PriceExclusionGate interface {
	Withholding(ctx context.Context, base, quote canonical.Asset, sources []string, from, to time.Time) pricingguard.Withholding
	InstantWithholding(ctx context.Context, base, quote canonical.Asset, source string, ts time.Time) pricingguard.Withholding
	UnattributedWithholding(ctx context.Context, base, quote canonical.Asset, from, to time.Time) pricingguard.Withholding
	MultiHopWithholding(ctx context.Context, from, to time.Time) pricingguard.Withholding
	// Snapshot is one read of the set, for a walk judging many buckets.
	Snapshot(ctx context.Context) (pricingguard.ExclusionSet, pricingguard.Withholding)
}

// bucketJudge is what [judgeBucket] needs: the gate, or one [pricingguard.ExclusionSet] read from it.
type bucketJudge interface {
	Withholding(ctx context.Context, base, quote canonical.Asset, sources []string, from, to time.Time) pricingguard.Withholding
	UnattributedWithholding(ctx context.Context, base, quote canonical.Asset, from, to time.Time) pricingguard.Withholding
}

// exclusionCleanTrades drops trades from an excluded source; the survivors are each one venue's own fill, so a VWAP over them
// is clean. An unreadable exclusion set is a withheld error, never "nothing excluded".
func (s *Server) exclusionCleanTrades(ctx context.Context, asset, quote canonical.Asset, trades []canonical.Trade) ([]canonical.Trade, error) {
	if s.Exclusions == nil {
		return trades, nil
	}
	kept := trades[:0:0]
	for _, t := range trades {
		switch w := s.Exclusions.InstantWithholding(ctx, asset, quote, t.Source, t.Timestamp); w {
		case pricingguard.NotWithheld:
			kept = append(kept, t)
		case pricingguard.WithheldSourceExcluded:
		default:
			return nil, PriceWithheldError(w)
		}
	}
	return kept, nil
}

// exclusionCleanWindow is [Server.exclusionCleanTrades] over a lifted raw-trade window for (base, quote). Each trade is judged
// on the book it executed on and on the requested pair: a fiat-quoted window pools stablecoin books that an exclusion stored
// under the fiat pair does not name.
func (s *Server) exclusionCleanWindow(ctx context.Context, base, quote canonical.Asset, window aggregate.ScaledWindow) (aggregate.ScaledWindow, error) {
	if s.Exclusions == nil {
		return window, nil
	}
	refused := pricingguard.NotWithheld
	kept := window.Filter(func(t canonical.Trade) bool {
		w := s.Exclusions.InstantWithholding(ctx, t.Pair.Base, t.Pair.Quote, t.Source, t.Timestamp)
		if w == pricingguard.NotWithheld && (!t.Pair.Base.Equal(base) || !t.Pair.Quote.Equal(quote)) {
			w = s.Exclusions.InstantWithholding(ctx, base, quote, t.Source, t.Timestamp)
		}
		if w != pricingguard.NotWithheld && w != pricingguard.WithheldSourceExcluded {
			refused = w
		}
		return w == pricingguard.NotWithheld
	})
	if refused != pricingguard.NotWithheld {
		return aggregate.ScaledWindow{}, PriceWithheldError(refused)
	}
	return kept, nil
}

// historyCleanPage drops the fills an exclusion covers from a /v1/history page; removed reports any were, so the caller marks
// the page degraded. The cursor is the read's, not the survivors', so paging continues past the window; only a page that
// empties with no next page is withheld, as an empty terminal page would read as a quiet market.
func (s *Server) historyCleanPage(
	ctx context.Context, base, quote canonical.Asset, trades []canonical.Trade, hasNext bool,
) (kept []canonical.Trade, removed bool, err error) {
	kept, err = s.exclusionCleanTrades(ctx, base, quote, trades)
	if err == nil && len(kept) == 0 && len(trades) > 0 && !hasNext {
		err = PriceWithheldError(pricingguard.WithheldSourceExcluded)
	}
	if err != nil {
		return nil, false, err
	}
	return kept, len(kept) < len(trades), nil
}

// writeExclusionCleanWindow is [Server.exclusionCleanWindow] for a handler: it writes the withheld problem (an emptied window
// included, which is excluded rather than "no trades") and returns ok=false. removed is for [markFillsWithheld].
func (s *Server) writeExclusionCleanWindow(
	w http.ResponseWriter, r *http.Request, base, quote canonical.Asset, window aggregate.ScaledWindow,
) (kept aggregate.ScaledWindow, removed, ok bool) {
	kept, err := s.exclusionCleanWindow(r.Context(), base, quote, window)
	if err == nil && kept.Len() == 0 && window.Len() > 0 {
		err = PriceWithheldError(pricingguard.WithheldSourceExcluded)
	}
	if err != nil {
		writePriceWithheldProblem(w, r, base, quote, priceWithheldReason(err))
		return aggregate.ScaledWindow{}, false, false
	}
	return kept, kept.Len() < window.Len(), true
}

// pegBucketWithheld judges a peg book's snapshot, served as asset/quote, under the requested pair over the span it pooled. A
// snapshot without sources cannot be checked source by source, so any exclusion touching the pair withholds it.
func (s *Server) pegBucketWithheld(ctx context.Context, asset, quote canonical.Asset, snap PriceSnapshot, sources []string) pricingguard.Withholding {
	if s.Exclusions == nil {
		return pricingguard.NotWithheld
	}
	to := snap.ObservedAt.Time()
	from := to.Add(-time.Duration(snap.WindowSeconds) * time.Second)
	if !from.Before(to) {
		to = from.Add(time.Nanosecond)
	}
	if len(sources) == 0 {
		return s.Exclusions.UnattributedWithholding(ctx, asset, quote, from, to)
	}
	return s.Exclusions.Withholding(ctx, asset, quote, sources, from, to)
}

// ohlcSeriesWithheld judges each bar over its own bucket under the requested pair and every market in books that fed that
// bucket (a fiat series pools stablecoin books an exclusion stored under the fiat pair does not name). A bar pools its sources
// and cannot be recomputed without one, so any covered bar withholds the series rather than serving a gap that reads as a quiet
// market. Sources are the bucket's union, so a source excluded on one book withholds even if it traded only on another.
func (s *Server) ohlcSeriesWithheld(ctx context.Context, pair canonical.Pair, bars []OHLCSeriesBar, books bucketBooks, interval ohlcInterval) error {
	if s.Exclusions == nil {
		return nil
	}
	for _, b := range bars {
		from := b.T.Time()
		to := from.Add(interval.duration())
		if interval == ohlcInterval1mo {
			to = from.AddDate(0, 1, 0)
		}
		if w := s.bucketWithholding(ctx, appendBook([]canonical.Pair{pair}, books[from]...), b.Sources, from, to); w != pricingguard.NotWithheld {
			return PriceWithheldError(w)
		}
	}
	return nil
}

// bucketWithholding judges one pooled bucket over [from, to) under each of books: on its recorded sources, or, with none
// recorded, by any exclusion naming the market. An empty span is widened to an instant.
func (s *Server) bucketWithholding(
	ctx context.Context, books []canonical.Pair, sources []string, from, to time.Time,
) pricingguard.Withholding {
	if s.Exclusions == nil {
		return pricingguard.NotWithheld
	}
	return judgeBucket(ctx, s.Exclusions, books, sources, from, to)
}

// judgeBucket is [Server.bucketWithholding] against j.
func judgeBucket(ctx context.Context, j bucketJudge, books []canonical.Pair, sources []string, from, to time.Time) pricingguard.Withholding {
	if !from.Before(to) {
		to = from.Add(time.Nanosecond)
	}
	for _, m := range books {
		var w pricingguard.Withholding
		if len(sources) > 0 {
			w = j.Withholding(ctx, m.Base, m.Quote, sources, from, to)
		} else {
			w = j.UnattributedWithholding(ctx, m.Base, m.Quote, from, to)
		}
		if w != pricingguard.NotWithheld {
			return w
		}
	}
	return pricingguard.NotWithheld
}

// judgeDirectBucket is [judgeBucket] for a bucket read from the books' own rows (a chart bucket): with no sources recorded
// (twap_* keeps none) it is judged by exclusions on those books, since no other market fed it.
func judgeDirectBucket(
	ctx context.Context, set pricingguard.ExclusionSet, books []canonical.Pair, sources []string, from, to time.Time,
) pricingguard.Withholding {
	if len(sources) > 0 {
		return judgeBucket(ctx, set, books, sources, from, to)
	}
	if !from.Before(to) {
		to = from.Add(time.Nanosecond)
	}
	for _, m := range books {
		if w := set.PairWithholding(ctx, m.Base, m.Quote, from, to); w != pricingguard.NotWithheld {
			return w
		}
	}
	return pricingguard.NotWithheld
}

// exclusionSnapshot reads the gate's set once for a many-bucket judgement; ok=false (nothing to judge) without a gate or with
// an empty set. Any verdict other than NotWithheld is the unread set, which withholds the whole series.
func (s *Server) exclusionSnapshot(ctx context.Context) (set pricingguard.ExclusionSet, ok bool, v pricingguard.Withholding) {
	if s.Exclusions == nil {
		return set, false, pricingguard.NotWithheld
	}
	set, v = s.Exclusions.Snapshot(ctx)
	return set, v == pricingguard.NotWithheld && !set.Empty(), v
}

// cleanPoints drops each chart bucket an exclusion covers over its own span, under the book it was read from and the walk's
// pair, so a gap stands where it was; the unread set refuses the series. A bucket another source already owns is never
// served from this one, so it is dropped unjudged and cannot mark the response.
func (w *chartWalk) cleanPoints(ctx context.Context, sp canonical.Pair, points []HistoryPoint) []HistoryPoint {
	if w.s.Exclusions == nil {
		return points
	}
	unowned := points[:0:0]
	for _, p := range points {
		if !w.merge.holds(p.Bucket) {
			unowned = append(unowned, p)
		}
	}
	if len(unowned) == 0 {
		return unowned
	}
	set, judge, v := w.s.exclusionSnapshot(ctx)
	if v != pricingguard.NotWithheld {
		w.res.refused = v
		return nil
	}
	if !judge {
		return unowned
	}
	books := appendBook([]canonical.Pair{sp}, append([]canonical.Pair{w.pair}, w.win.servedAs...)...)
	kept := unowned[:0:0]
	for _, p := range unowned {
		if judgeDirectBucket(ctx, set, books, p.Sources, p.Bucket, chartBucketStep(p.Bucket, w.win.gran)) == pricingguard.NotWithheld {
			kept = append(kept, p)
		} else {
			w.res.withheldFrom = earliestWithheld(w.res.withheldFrom, p.Bucket)
		}
	}
	return kept
}

// earliestWithheld is the earlier of two withheld-bucket starts, zero meaning none.
func earliestWithheld(a, b time.Time) time.Time {
	if a.IsZero() || (!b.IsZero() && b.Before(a)) {
		return b
	}
	return a
}

// fxSeriesClean drops each daily fx_quotes point an exclusion covers, judged on its vendor over its day (none recorded: any
// exclusion naming the ticker) under ticker/USD and each of also (the pair a cross is served as); withheldFrom is the first
// day dropped, zero when none was. The unread set is a withheld error for the whole series.
func (s *Server) fxSeriesClean(
	ctx context.Context, ticker string, points []FXQuotePoint, also ...canonical.Pair,
) (kept []FXQuotePoint, withheldFrom time.Time, err error) {
	if ticker == "USD" || len(points) == 0 {
		return points, time.Time{}, nil
	}
	set, judge, v := s.exclusionSnapshot(ctx)
	if v != pricingguard.NotWithheld {
		return nil, time.Time{}, PriceWithheldError(v)
	}
	if !judge {
		return points, time.Time{}, nil
	}
	books := appendBook([]canonical.Pair{{Base: canonical.Asset{Type: canonical.AssetFiat, Code: ticker}, Quote: defaultPriceQuote}}, also...)
	kept = points[:0:0]
	for _, p := range points {
		var sources []string
		if p.Source != "" {
			sources = []string{p.Source}
		}
		if judgeBucket(ctx, set, books, sources, p.Bucket, p.Bucket.Add(24*time.Hour)) == pricingguard.NotWithheld {
			kept = append(kept, p)
		} else {
			withheldFrom = earliestWithheld(withheldFrom, p.Bucket)
		}
	}
	return kept, withheldFrom, nil
}

// markFillsWithheld puts an exclusion's removal of fills, or of the buckets built from them, on the wire; without it the answer
// is the same body as a quieter market. Degraded keeps it out of shared caches so lifting the exclusion is seen at once.
func markFillsWithheld(f Flags, removed bool) Flags {
	if removed {
		f.FillsWithheldReason, f.Degraded = PriceWithheldSourceExcluded, true
	}
	return f
}

// writeChartWithheld writes the withheld problem for a chart read that refused the whole series; false when err is not one.
func writeChartWithheld(w http.ResponseWriter, r *http.Request, pair canonical.Pair, err error) bool {
	if !errors.Is(err, ErrPriceWithheld) {
		return false
	}
	writePriceWithheldProblem(w, r, pair.Base, pair.Quote, priceWithheldReason(err))
	return true
}

// readPriceAt is the [PriceAtReader] read every point-in-time walk makes, with a bucket an exclusion covers, on book or on any
// of also (the requested pair a stablecoin fallback stands in for), returned as a withheld-class miss.
func (s *Server) readPriceAt(
	ctx context.Context, book canonical.Pair, ts time.Time, maxStaleness time.Duration, also ...canonical.Pair,
) (string, time.Time, int, error) {
	value, observedAt, resSec, sources, err := s.PriceAt.PriceAt(ctx, book, ts, maxStaleness)
	for _, p := range append([]canonical.Pair{book}, also...) {
		if err == nil {
			err = s.priceAtWithheld(ctx, p, observedAt, resSec, sources)
		}
	}
	if err != nil {
		return "", time.Time{}, 0, err
	}
	return value, observedAt, resSec, nil
}

// priceAtWithheld judges a point-in-time CAGG bucket closing at observedAt under pair over the whole span it pooled, on the
// bucket's sources; a bucket with none recorded cannot be checked source by source, so any exclusion touching the pair withholds it.
func (s *Server) priceAtWithheld(ctx context.Context, pair canonical.Pair, observedAt time.Time, resSec int, sources []string) error {
	from := observedAt.Add(-time.Duration(resSec) * time.Second)
	if w := s.bucketWithholding(ctx, []canonical.Pair{pair}, sources, from, observedAt); w != pricingguard.NotWithheld {
		return PriceWithheldError(w)
	}
	return nil
}

// fxRateWithheld judges one FX rate, source's own reading of ticker per USD over [from, to); nil when no exclusion covers it.
func (s *Server) fxRateWithheld(ctx context.Context, ticker, source string, from, to time.Time) error {
	if s.Exclusions == nil || ticker == "USD" {
		return nil
	}
	fiat := canonical.Asset{Type: canonical.AssetFiat, Code: ticker}
	if w := s.Exclusions.Withholding(ctx, fiat, defaultPriceQuote, []string{source}, from, to); w != pricingguard.NotWithheld {
		return PriceWithheldError(w)
	}
	return nil
}

// fixingWithheld is [Server.fxRateWithheld] over a bound fixing's bar, which pools the source's readings across it.
func (s *Server) fixingWithheld(ctx context.Context, ticker string, b timescale.FXFixingBinding) error {
	from, to := b.BarStart, b.BarEnd
	if !from.Before(to) {
		from, to = b.BarEnd, b.BarEnd.Add(time.Nanosecond)
	}
	return s.fxRateWithheld(ctx, ticker, b.Source, from, to)
}

// liveRatesWithheld is [Server.liveRateWithheld] over every leg of a cross; the first covered leg withholds it.
func (s *Server) liveRatesWithheld(ctx context.Context, legs []CurrencyEntry, publishedAt time.Time) error {
	for _, c := range legs {
		if err := s.liveRateWithheld(ctx, c, publishedAt); err != nil {
			return err
		}
	}
	return nil
}

// liveRateWithheld is [Server.fxRateWithheld] for a forex snapshot entry at its own update time.
func (s *Server) liveRateWithheld(ctx context.Context, c CurrencyEntry, publishedAt time.Time) error {
	at := c.UpdatedAt
	if at.IsZero() {
		at = publishedAt
	}
	return s.fxRateWithheld(ctx, c.Ticker, fxSourceOf(c), at, at.Add(time.Nanosecond))
}

// priceWithheldUnread reports whether err withholds because the exclusion set could not be read: a transient refusal that a
// cached body must not replay as a clean withheld one.
func priceWithheldUnread(err error) bool {
	var e *withheldError
	return errors.As(err, &e) && e.unread
}

// unattributedWithheld judges a value with no source list that covers [from, now]. multiHop marks a value that may be a
// composite over unrecorded legs (a triangulated cache value, or one whose provenance is unknown): any exclusion withholds it.
func (s *Server) unattributedWithheld(ctx context.Context, asset, quote canonical.Asset, from time.Time, multiHop bool) pricingguard.Withholding {
	if s.Exclusions == nil {
		return pricingguard.NotWithheld
	}
	to := time.Now().Add(time.Nanosecond)
	if multiHop {
		return s.Exclusions.MultiHopWithholding(ctx, from, to)
	}
	return s.Exclusions.UnattributedWithholding(ctx, asset, quote, from, to)
}
