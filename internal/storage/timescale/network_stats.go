package timescale

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// NetworkStats is the aggregate counts powering the home network
// strip on stellarindex.io. Single SQL query computes them all so
// the explorer doesn't need 4 separate fetches.
type NetworkStats struct {
	// Volume24hUSD: SUM of prices_1m.volume_usd across every pair
	// over the trailing 24h. Decimal string per ADR-0003 (the
	// volume column is NUMERIC and can exceed int64 in raw cents).
	// Nil when prices_1m has no recent rows.
	Volume24hUSD *string
	// MarketsCount24h: distinct (base, quote) pairs that recorded any volume_usd in the trailing
	// 24h. It is an ORDER OF MAGNITUDE SMALLER than AssetsIndexed because it counts only pairs
	// that TRADED in the window, while AssetsIndexed counts every classic asset ever seen.
	MarketsCount24h int64
	// AssetsIndexed: total rows in classic_assets. It grows monotonically and does not filter by
	// recent activity: it is "what we know about", not "what's currently trading".
	//
	// EXPECT A STEP CHANGE the first time `stellarindex-ops asset-registry-backfill` runs; that
	// is not an anomaly or double counting. Without the backfill the registry learns of an asset
	// only from a TRADE, so most of the trustline population is missing and the number means
	// "assets that have traded at least once".
	AssetsIndexed int64
	// LatestLedger: max(ingestion_cursors.last_ledger) across the live
	// cursor namespaces ([LiveCursorSources]). A one-shot job's shard
	// cursor marks the end of a historical range, not the live tip.
	LatestLedger int64
}

// GetNetworkStats returns the home-page aggregate stats in one call.
// Cheap query: hypertable-bound time filter for volume; one COUNT
// for distinct markets; one COUNT for classic_assets; one MAX for
// the live cursor.
func (s *Store) GetNetworkStats(ctx context.Context) (NetworkStats, error) {
	q := networkStatsQuery()
	var (
		out    NetworkStats
		volStr sql.NullString
	)
	if err := s.db.QueryRowContext(ctx, q, liveCursorSources, canonical.NativeSACContractID()).Scan(
		&volStr,
		&out.MarketsCount24h,
		&out.AssetsIndexed,
		&out.LatestLedger,
	); err != nil {
		return NetworkStats{}, fmt.Errorf("timescale: GetNetworkStats: %w", err)
	}
	if volStr.Valid {
		v := volStr.String
		out.Volume24hUSD = &v
	}
	return out, nil
}

// networkStatsQuery builds GetNetworkStats's query. canonBase/canonQuote fold a market's two
// stored orientations (e.g. XLM/USDC and USDC/XLM) into one row before counting distinct pairs
// (see canonOrientSQL), so a market recorded in both directions is not counted twice.
func networkStatsQuery() string {
	canonBase, canonQuote, _ := canonOrientSQL(2)
	return `
		SELECT
		  (SELECT SUM(volume_usd)::text FROM prices_1m
		    WHERE bucket >= now() - INTERVAL '24 hours'
		      AND volume_usd IS NOT NULL)                      AS volume_24h_usd,
		  (SELECT COUNT(*)::bigint FROM (
		     SELECT DISTINCT ` + canonBase + ` AS base_asset, ` + canonQuote + ` AS quote_asset
		       FROM prices_1m
		      WHERE bucket >= now() - INTERVAL '24 hours'
		        AND volume_usd IS NOT NULL
		   ) t)                                                AS markets_count_24h,
		  (SELECT COUNT(*)::bigint FROM classic_assets)        AS assets_indexed,
		  COALESCE(
		    (SELECT MAX(last_ledger)::bigint FROM ingestion_cursors
		      WHERE source = ANY($1)),
		    0
		  )                                                    AS latest_ledger
	`
}
