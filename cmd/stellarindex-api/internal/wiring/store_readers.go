package wiring

import (
	"context"
	"errors"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// StoreVolumeReader adapts *timescale.Store to v1.VolumeReader.
// Returns the trailing-24h USD volume across every pair the asset
// participates in. No error translation needed — the timescale
// helper returns "0" when the asset is tracked but had no trades,
// and a real error for genuine SQL failures.
type StoreVolumeReader struct{ S *timescale.Store }

func (r StoreVolumeReader) Volume24hUSDForAsset(ctx context.Context, assetKey string) (string, bool, error) {
	return r.S.Volume24hUSDForAsset(ctx, assetKey)
}

// SorobanVolume24hUSDForAsset implements the optional
// v1.SorobanVolumeReader — the XLM-anchored 24h USD-volume variant used
// for pure-Soroban SEP-41 assets whose liquidity is quoted in XLM rather
// than a USD-pegged classic (fce3e2eef).
func (r StoreVolumeReader) SorobanVolume24hUSDForAsset(ctx context.Context, assetKey string) (string, bool, error) {
	return r.S.SorobanVolume24hUSDForAsset(ctx, assetKey)
}

// StoreSupplyLooker adapts *timescale.Store to v1.SupplyLooker for
// the F2-fields path on /v1/assets/{id}.
//
// Error translation: timescale.ErrNotFound (no recorded snapshot)
// becomes v1.ErrSupplyNotFound, which the handler treats as
// "feature unavailable for this asset" and leaves the F2 fields
// null on the response. Other errors propagate unchanged so the
// handler can log them at WARN.
type StoreSupplyLooker struct{ S *timescale.Store }

func (r StoreSupplyLooker) LatestSupply(ctx context.Context, assetKey string) (supply.Supply, error) {
	snap, err := r.S.LatestSupply(ctx, assetKey)
	if err != nil {
		if errors.Is(err, timescale.ErrNotFound) {
			return supply.Supply{}, v1.ErrSupplyNotFound
		}
		return supply.Supply{}, err
	}
	return snap, nil
}

// SupplyCoverageStats delegates to the underlying Store so the
// wrapper satisfies v1.SupplyCoverageReader as well as
// v1.SupplyLooker. Same pattern as FXHistoryReader's coverage
// delegate — without it, /v1/diagnostics/ingestion's supply
// section renders as empty.
func (r StoreSupplyLooker) SupplyCoverageStats(ctx context.Context) (timescale.SupplyCoverage, error) {
	return r.S.SupplyCoverageStats(ctx)
}

// DailyCirculatingSupply delegates to the Store's supply_1d CAGG
// reader (migration 0066), the supply leg of crypto market-cap-over-
// time on /v1/chart?price_type=market_cap.
func (r StoreSupplyLooker) DailyCirculatingSupply(ctx context.Context, assetKey string, from, to time.Time) ([]timescale.SupplyDayPoint, error) {
	return r.S.DailyCirculatingSupply(ctx, assetKey, from, to)
}

// FXHistoryReader adapts (*timescale.Store) to v1.FXHistoryReader.
// Mirrors the writer adapter but on the read path; the v1 package's
// FXQuotePoint deliberately omits Ticker + Source (the handler
// already knows ticker, source is provenance not display data).
type FXHistoryReader struct{ Store *timescale.Store }

func (r *FXHistoryReader) ListFXHistory(ctx context.Context, ticker string, from, to time.Time) ([]v1.FXQuotePoint, error) {
	rows, err := r.Store.ListFXHistory(ctx, ticker, from, to)
	if err != nil {
		return nil, err
	}
	out := make([]v1.FXQuotePoint, len(rows))
	for i, q := range rows {
		out[i] = v1.FXQuotePoint{
			Bucket:         q.Bucket,
			RateUSDText:    q.RateUSDText,
			InverseUSDText: q.InverseUSDText,
		}
	}
	return out, nil
}

// FXCoverageStats delegates to the underlying Store so the wrapper
// satisfies v1.FXCoverageReader as well as v1.FXHistoryReader. The
// /v1/diagnostics/ingestion endpoint type-asserts to FXCoverageReader
// at request time; if this delegate is missing, the FX section of
// the response renders as empty.
func (r *FXHistoryReader) FXCoverageStats(ctx context.Context) (timescale.FXCoverage, error) {
	return r.Store.FXCoverageStats(ctx)
}

// CAGGCoverageStats delegates so the wrapper satisfies
// v1.CAGGCoverageReader too. Same pattern — without the delegate,
// the prices_1h coverage section on /v1/diagnostics/ingestion
// renders empty.
func (r *FXHistoryReader) CAGGCoverageStats(ctx context.Context) (timescale.CAGGCoverage, error) {
	return r.Store.CAGGCoverageStats(ctx)
}

// SourceEntryCounts delegates so the wrapper satisfies
// v1.SourceEntryCountReader too. Same pattern as the two above —
// and the one that bit us: without this delegate the type
// assertion in fillIngestionEntryCounts fails closed and the
// `entries` column on /v1/diagnostics/ingestion is silently 0 for
// EVERY source, even though source_entry_counts (migration 0035,
// maintained live by the indexer + seed-entry-counts) is fully
// populated. Shipped missing in rc.55; entries read 0 on the
// status page until this landed.
func (r *FXHistoryReader) SourceEntryCounts(ctx context.Context) (map[string]int64, error) {
	return r.Store.SourceEntryCounts(ctx)
}
