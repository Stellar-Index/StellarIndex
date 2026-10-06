package wiring

import (
	"context"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// StoreHistoryReader adapts *timescale.Store to v1.HistoryReader.
// Pure passthrough: the store already returns []canonical.Trade
// ordered by ts ASC, which is exactly what the handler expects.
type StoreHistoryReader struct{ S *timescale.Store }

func (r StoreHistoryReader) TradesInRange(ctx context.Context, pair canonical.Pair, from, to time.Time, limit int) ([]canonical.Trade, error) {
	return r.S.TradesInRange(ctx, pair, from, to, limit)
}

func (r StoreHistoryReader) TradesInRangeAfter(ctx context.Context, pair canonical.Pair, from, to, afterTs time.Time, afterLedger uint32, afterTxHash, afterSource string, afterOpIndex uint32, limit int) ([]canonical.Trade, error) {
	return r.S.TradesInRangeAfter(ctx, pair, from, to, afterTs, afterLedger, afterTxHash, afterSource, afterOpIndex, limit)
}

func (r StoreHistoryReader) TradesInRangeAfterFromSource(ctx context.Context, pair canonical.Pair, source string, from, to, afterTs time.Time, afterLedger uint32, afterTxHash, afterSource string, afterOpIndex uint32, limit int) ([]canonical.Trade, error) {
	return r.S.TradesInRangeAfterFromSource(ctx, pair, source, from, to, afterTs, afterLedger, afterTxHash, afterSource, afterOpIndex, limit)
}

// LatestTradePerSource adapts [timescale.Store.LatestTradePerSource]
// to the v1.HistoryReader interface. Pure passthrough: the store
// already does the DISTINCT ON (source) work in SQL.
func (r StoreHistoryReader) LatestTradePerSource(ctx context.Context, pair canonical.Pair, sourceFilter string) ([]canonical.Trade, error) {
	return r.S.LatestTradePerSource(ctx, pair, sourceFilter)
}

// HistoryPoints adapts [timescale.Store.HistoryPoints] to the
// v1.HistoryReader interface. Translates the storage-side
// timescale.HistoryGranularity string-typed enum back to plain
// strings for the v1 type, and the rich timescale.HistoryPoint to
// the v1 wire-shape variant. Unknown granularities propagate as
// v1.ErrUnknownGranularity (handler turns into 400).
func (r StoreHistoryReader) HistoryPoints(ctx context.Context, pair canonical.Pair, granularity string, limit int) ([]v1.HistoryPoint, error) {
	g := timescale.HistoryGranularity(granularity)
	if err := g.Validate(); err != nil {
		return nil, v1.ErrUnknownGranularity
	}
	rows, err := r.S.HistoryPoints(ctx, pair, g, limit)
	if err != nil {
		return nil, err
	}
	return convertHistoryPoints(rows), nil
}

// HistoryPointsInRange adapts [timescale.Store.HistoryPointsInRange]
// to the v1.HistoryReader interface. Same translation rules as
// [StoreHistoryReader.HistoryPoints]; passes the from/to window
// through to the storage layer.
func (r StoreHistoryReader) HistoryPointsInRange(ctx context.Context, pair canonical.Pair, granularity string, from, to time.Time, limit int) ([]v1.HistoryPoint, error) {
	g := timescale.HistoryGranularity(granularity)
	if err := g.Validate(); err != nil {
		return nil, v1.ErrUnknownGranularity
	}
	rows, err := r.S.HistoryPointsInRange(ctx, pair, g, from, to, limit)
	if err != nil {
		return nil, err
	}
	return convertHistoryPoints(rows), nil
}

// StoreCoverageFloorReader adapts *timescale.Store to
// v1.CoverageFloorReader. Separate from [StoreHistoryReader] on
// purpose: the serving reader is wrapped in a 2-minute SWR cache whose
// keying is per-method, while the floor has its own TTL memo in the
// handler layer keyed by the pair's alias-canonical identity — layering
// one over the other would cache the same answer twice under different
// keys. Translates the string-typed granularity to the storage enum;
// an unknown value surfaces as the store's own validation error, which
// the handler renders as "no signal".
type StoreCoverageFloorReader struct{ S *timescale.Store }

func (r StoreCoverageFloorReader) EarliestBucket(ctx context.Context, pair canonical.Pair, granularity string, from, to time.Time) (time.Time, bool, error) {
	return r.S.EarliestBucket(ctx, pair, timescale.HistoryGranularity(granularity), from, to)
}

func (r StoreCoverageFloorReader) EarliestBucketAsStored(ctx context.Context, pair canonical.Pair, granularity string, from, to time.Time) (time.Time, bool, error) {
	return r.S.EarliestBucketAsStored(ctx, pair, timescale.HistoryGranularity(granularity), from, to)
}

func (r StoreCoverageFloorReader) EarliestBucketLiteralQuote(ctx context.Context, pair canonical.Pair, granularity string, from, to time.Time) (time.Time, bool, error) {
	return r.S.EarliestBucketLiteralQuote(ctx, pair, timescale.HistoryGranularity(granularity), from, to)
}

// TWAPPointsInRange adapts [timescale.Store.TWAPPointsInRange] to the
// v1.HistoryReader interface. Only 1h / 1d have a TWAP CAGG
// (migration 0081); any other granularity propagates as
// v1.ErrUnknownGranularity (handler turns into 400).
func (r StoreHistoryReader) TWAPPointsInRange(ctx context.Context, pair canonical.Pair, granularity string, from, to time.Time, limit int) ([]v1.HistoryPoint, error) {
	g := timescale.HistoryGranularity(granularity)
	if !timescale.TWAPGranularitySupported(g) {
		return nil, v1.ErrUnknownGranularity
	}
	rows, err := r.S.TWAPPointsInRange(ctx, pair, g, from, to, limit)
	if err != nil {
		return nil, err
	}
	return convertHistoryPoints(rows), nil
}

// OHLCSeries adapts [timescale.Store.OHLCSeries] /
// [timescale.Store.OHLCSeriesReBucketed] to the v1.HistoryReader
// interface. The interval → view decision is [timescale.OHLCRoutes]:
// a native row reads its own CAGG, a folded row re-buckets a finer
// one (5m/30m from prices_1m, 2h/4h/12h from prices_1h, 3d from
// prices_1d, 2w from prices_1w). Nothing is decided here, so the
// reader cannot route an interval the store's fold allow-list has
// not declared — the drift that 500d 2h/12h/3d/2w. Unknown
// intervals propagate as v1.ErrUnknownGranularity.
func (r StoreHistoryReader) OHLCSeries(ctx context.Context, pair canonical.Pair, interval string, from, to time.Time, limit int) ([]v1.OHLCSeriesBar, error) {
	route, ok := timescale.OHLCRouteFor(interval)
	if !ok {
		return nil, v1.ErrUnknownGranularity
	}
	var (
		bars []timescale.OHLCBar
		err  error
	)
	if route.Folded() {
		bars, err = r.S.OHLCSeriesReBucketed(ctx, pair, route.Source, route.Fold, from, to, limit)
	} else {
		bars, err = r.S.OHLCSeries(ctx, pair, route.Native, from, to, limit)
	}
	if err != nil {
		return nil, err
	}
	return convertOHLCBars(bars), nil
}

func convertOHLCBars(bars []timescale.OHLCBar) []v1.OHLCSeriesBar {
	out := make([]v1.OHLCSeriesBar, len(bars))
	for i, b := range bars {
		out[i] = v1.OHLCSeriesBar{
			T:       v1.WireTime(b.Bucket),
			O:       b.Open,
			H:       b.High,
			L:       b.Low,
			C:       b.Close,
			VBase:   b.BaseVolume,
			VQuote:  b.QuoteVolume,
			N:       b.TradeCount,
			Sources: b.Sources,
		}
	}
	return out
}

func convertHistoryPoints(rows []timescale.HistoryPoint) []v1.HistoryPoint {
	out := make([]v1.HistoryPoint, len(rows))
	for i, row := range rows {
		out[i] = v1.HistoryPoint{
			Bucket:    row.Bucket,
			VWAP:      row.VWAP,
			VolumeUSD: row.VolumeUSD,
			Sources:   row.Sources,
		}
	}
	return out
}
