package timescale

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// FXCoverage is the storage-layer projection of how much FX-history
// the fx_quotes hypertable currently holds. Powers the
// /v1/diagnostics/ingestion surface so operators can see at a glance
// whether the Frankfurter / Massive backfill ran to completion +
// whether the daily live-write tick is keeping up.
//
// Empty / zero-valued when fx_quotes has no rows yet — handlers
// project that as "—" rather than an error.
type FXCoverage struct {
	// EarliestQuote / LatestQuote are MIN/MAX(bucket). Zero
	// values when fx_quotes is empty.
	EarliestQuote time.Time
	LatestQuote   time.Time
	// TotalQuotes is the total row count across all tickers +
	// dates. Useful for sanity-checking against an expected
	// "tickers × days" multiplier.
	TotalQuotes int64
	// CurrenciesCount is COUNT(DISTINCT ticker) — i.e. how many
	// distinct fiat currencies have at least one quote.
	CurrenciesCount int
}

// FXCoverageStats returns the current coverage state of the
// fx_quotes hypertable. A single GROUPING() aggregate over the
// hypertable; cheap (~1ms on a populated table because the
// hypertable's index sits on bucket).
func (s *Store) FXCoverageStats(ctx context.Context) (FXCoverage, error) {
	const q = `
		SELECT
		    MIN(bucket),
		    MAX(bucket),
		    COUNT(*),
		    COUNT(DISTINCT ticker)
		FROM fx_quotes
	`
	var (
		minB, maxB sql.NullTime
		total      int64
		currencies int
	)
	if err := s.db.QueryRowContext(ctx, q).Scan(&minB, &maxB, &total, &currencies); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return FXCoverage{}, nil
		}
		return FXCoverage{}, err
	}
	out := FXCoverage{TotalQuotes: total, CurrenciesCount: currencies}
	if minB.Valid {
		out.EarliestQuote = minB.Time
	}
	if maxB.Valid {
		out.LatestQuote = maxB.Time
	}
	return out, nil
}

// SupplyCoverage is the storage-layer projection of how many assets
// have a supply snapshot today + how recently the most recent
// snapshot was written. Powers the ingestion-diagnostics surface so
// operators can spot a stalled supply observer
// (LastSnapshotAt > a few minutes ago) without paging through
// asset_supply_history by hand.
//
// "Classic" vs "SEP-41" splits asset_key by prefix — SEP-41 contract
// IDs start with "C", classic assets are "native" or
// "CODE:G-strkey". The split mirrors the three-domain supply
// algorithm split (XLM / classic / SEP-41).
type SupplyCoverage struct {
	ClassicAssets  int
	SEP41Assets    int
	LastSnapshotAt time.Time
	LatestLedger   int64
}

// SupplyCoverageStats returns the current coverage state of the
// asset_supply_history hypertable. One window-function query that
// reads the latest row per asset_key and partitions by SEP-41 vs
// classic. The btree index on (asset_key, ledger_sequence, time)
// makes the DISTINCT ON cheap.
func (s *Store) SupplyCoverageStats(ctx context.Context) (SupplyCoverage, error) {
	const q = `
		WITH latest AS (
		    SELECT DISTINCT ON (asset_key)
		        asset_key, time, ledger_sequence
		    FROM asset_supply_history
		    ORDER BY asset_key, ledger_sequence DESC, time DESC
		)
		SELECT
		    COUNT(*) FILTER (WHERE asset_key LIKE 'C%' AND LENGTH(asset_key) = 56) AS sep41,
		    COUNT(*) FILTER (WHERE NOT (asset_key LIKE 'C%' AND LENGTH(asset_key) = 56)) AS classic,
		    MAX(time)         AS last_at,
		    MAX(ledger_sequence) AS last_ledger
		FROM latest
	`
	var (
		sep41, classic int
		lastAt         sql.NullTime
		lastLedger     sql.NullInt64
	)
	if err := s.db.QueryRowContext(ctx, q).Scan(&sep41, &classic, &lastAt, &lastLedger); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SupplyCoverage{}, nil
		}
		return SupplyCoverage{}, err
	}
	out := SupplyCoverage{
		ClassicAssets: classic,
		SEP41Assets:   sep41,
	}
	if lastAt.Valid {
		out.LastSnapshotAt = lastAt.Time
	}
	if lastLedger.Valid {
		out.LatestLedger = lastLedger.Int64
	}
	return out, nil
}
