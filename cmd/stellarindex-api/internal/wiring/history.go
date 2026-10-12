package wiring

import (
	"context"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// StoreHistoryReader adapts *timescale.Store to v1.HistoryReader.
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

// LatestTradePerSource adapts [timescale.Store.LatestTradePerSource] to the v1.HistoryReader interface.
func (r StoreHistoryReader) LatestTradePerSource(ctx context.Context, pair canonical.Pair, sourceFilter string) ([]canonical.Trade, error) {
	return r.S.LatestTradePerSource(ctx, pair, sourceFilter)
}

// HistoryPoints adapts [timescale.Store.HistoryPoints] to v1.HistoryReader, converting the storage granularity enum and HistoryPoint to the v1 wire types.
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

// HistoryPointsInRange adapts [timescale.Store.HistoryPointsInRange] to the v1.HistoryReader interface.
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

// StoreCoverageFloorReader adapts *timescale.Store to v1.CoverageFloorReader.
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

// TWAPPointsInRange adapts [timescale.Store.TWAPPointsInRange] to the v1.HistoryReader interface; only 1h / 1d have a TWAP CAGG, other granularities propagate as v1.ErrUnknownGranularity.
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

// OHLCSeries adapts [timescale.Store.OHLCSeries] / [timescale.Store.OHLCSeriesReBucketed] to v1.HistoryReader; interval routing is decided by [timescale.OHLCRoutes], not here, and unknown intervals propagate as v1.ErrUnknownGranularity.
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
