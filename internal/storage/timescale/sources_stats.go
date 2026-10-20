package timescale

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// SourceStats is the per-source 24h activity row.
type SourceStats struct {
	Source        string
	TradeCount24h int64
	// VolumeUSD24h is SUM(usd_volume) over trades in the trailing 24h, a numeric string to keep
	// precision on the wire. "" when no trade in the window had usd_volume populated.
	VolumeUSD24h sql.NullString
	// UnpricedTrades24h counts trades with no
	// usd_volume. They are excluded from VolumeUSD24h, which is then a
	// lower bound.
	UnpricedTrades24h int64
	// MarketsCount24h is the number of distinct markets the source
	// observed in the trailing 24h, a market being its unordered
	// {base, quote} pair (see marketKeySQL). A useful "pools per DEX"
	// proxy for AMMs where each pair contract = one pool.
	MarketsCount24h int64
}

// GetSourceStats returns trailing-24h trade counts and volume grouped by source; sources with no trades are absent, and volume is SUM(usd_volume) stamped at trade time, so unvalued trades are never repriced at today's rate.
func (s *Store) GetSourceStats(ctx context.Context) ([]SourceStats, error) {
	q := sourceStatsQuery()
	rows, err := s.db.QueryContext(ctx, q, canonical.NativeSACContractID())
	if err != nil {
		return nil, fmt.Errorf("timescale: GetSourceStats: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []SourceStats
	for rows.Next() {
		var ss SourceStats
		if err := rows.Scan(
			&ss.Source,
			&ss.TradeCount24h,
			&ss.VolumeUSD24h,
			&ss.MarketsCount24h,
			&ss.UnpricedTrades24h,
		); err != nil {
			return nil, fmt.Errorf("timescale: GetSourceStats scan: %w", err)
		}
		out = append(out, ss)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: GetSourceStats rows: %w", err)
	}
	return out, nil
}

// sourceStatsQuery builds GetSourceStats's query; canonBase/canonQuote fold a market's two orientations into one per_pair group (see
// canonOrientSQL) so markets_24h is not inflated per flipped pair.
func sourceStatsQuery() string {
	canonBase, canonQuote, _ := canonOrientSQL(1)
	return `
		-- Two-level aggregate. The natural form of this
		-- query — a single GROUP BY source carrying
		-- COUNT(DISTINCT (base_asset, quote_asset)) — measured 15.1 s on
		-- production, because COUNT(DISTINCT) forces a per-group SORT of
		-- every row in the 24 h window (~6M rows). Decomposed on R1:
		--
		--   count(*) GROUP BY source ............... 0.94 s
		--   + COUNT(DISTINCT (base,quote)) ........ 14.20 s   <- 93% of cost
		--   + SUM(usd_volume) ..................... 15.10 s
		--
		-- Grouping once on (source, base_asset, quote_asset) lets the
		-- planner hash-aggregate instead of sorting, and the distinct
		-- market count then falls out as a plain COUNT(*) over the inner
		-- groups. Identical results, measured 4.7 s — a 3.2x improvement
		-- that brings this back under the handler's 8 s ceiling, which it
		-- had been silently blowing on every request (see below).
		--
		-- The orientation fold runs over the stored-column groups, not every
		-- row: grouping on the fold expression misestimates the group count
		-- and spills to an external sort.
		SELECT source,
		       SUM(pair_trades)::bigint AS trades_24h,
		       SUM(pair_volume)::text   AS volume_usd_24h,
		       COUNT(*)::bigint         AS markets_24h,
		       SUM(pair_unpriced)::bigint AS unpriced_trades_24h
		  FROM (
		    SELECT source,
		           ` + canonBase + ` AS base_asset,
		           ` + canonQuote + ` AS quote_asset,
		           SUM(raw_trades) AS pair_trades,
		           SUM(raw_volume) AS pair_volume,
		           SUM(raw_unpriced) AS pair_unpriced
		      FROM (
		        SELECT source, base_asset, quote_asset,
		               COUNT(*) AS raw_trades,
		               SUM(usd_volume::numeric) AS raw_volume,
		               COUNT(*) FILTER (WHERE usd_volume IS NULL) AS raw_unpriced
		          FROM trades
		         WHERE ts >= now() - INTERVAL '24 hours'
		         GROUP BY source, base_asset, quote_asset
		      ) raw
		     GROUP BY source, ` + canonBase + `, ` + canonQuote + `
		  ) per_pair
		 GROUP BY source
		 ORDER BY 2 DESC
	`
}

// SourceVolumeBucket is one hour-resolution USD-volume datapoint for a single source.
type SourceVolumeBucket struct {
	Source     string
	Hour       time.Time
	VolumeUSD  string // numeric stringified for precision parity
	TradeCount int64
	// XLMUnpriced is true when the bucket holds XLM-leg trades with no
	// usd_volume; they are excluded from VolumeUSD, a lower bound.
	XLMUnpriced bool
}

// GetSourceVolumeHistory24h returns one row per (source, hour) for the trailing 24h, for per-source sparklines.
func (s *Store) GetSourceVolumeHistory24h(ctx context.Context) ([]SourceVolumeBucket, error) {
	return s.sourceVolumeHistory(ctx, "24 hours")
}

// GetSourceVolumeHistory7d is the 7-day variant (168 hourly buckets per source), read from the source_volume_1h CAGG because deriving from raw SDEX trades overran the API's 8s ceiling.
func (s *Store) GetSourceVolumeHistory7d(ctx context.Context) ([]SourceVolumeBucket, error) {
	return s.sourceVolumeHistory(ctx, "7 days")
}

// sourceVolumeHistoryQuery returns per-(source, hour) volume over a trailing `window` bound as $1. It reads source_volume_1h: unpriced
// XLM legs surface as xlm_unpriced, not valued at today's rate; the bucket predicate is dexHourlyBucketWindow, shared with the DEX 24h
// readers.
func sourceVolumeHistoryQuery() string {
	return `
		SELECT source,
		       bucket AS hour,
		       COALESCE(sum_usd_priced, 0)::text AS volume_usd,
		       trade_count::bigint AS trade_count,
		       (COALESCE(sum_xlm_base, 0) + COALESCE(sum_xlm_quote, 0)) > 0 AS xlm_unpriced
		  FROM source_volume_1h
		 WHERE ` + dexHourlyBucketWindow(1) + `
		 ORDER BY source, hour
	`
}

func (s *Store) sourceVolumeHistory(ctx context.Context, window string) ([]SourceVolumeBucket, error) {
	rows, err := s.db.QueryContext(ctx, sourceVolumeHistoryQuery(), window)
	if err != nil {
		return nil, fmt.Errorf("timescale: sourceVolumeHistory: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []SourceVolumeBucket
	for rows.Next() {
		var b SourceVolumeBucket
		if err := rows.Scan(&b.Source, &b.Hour, &b.VolumeUSD, &b.TradeCount, &b.XLMUnpriced); err != nil {
			return nil, fmt.Errorf("timescale: sourceVolumeHistory scan: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: sourceVolumeHistory rows: %w", err)
	}
	return out, nil
}

// The per-source breakdowns: two fully static query strings (not concatenated, gosec G202)
// differing only in the WHERE predicate. Volume and unpriced handling match GetSourceStats.
//
// Asset filters use `= ANY($n)` against a bound string[] so the handler can pass every canonical
// FORM of an asset (XLM's three: `native`, `crypto:XLM`, the SAC; see [canonical.AssetAliases]).
// One form undercounts: SDEX writes `native`, CEXes `crypto:XLM`, Soroban AMMs the SAC. It stays
// sargable.
//
// The pair query reads BOTH stored orientations, one UNION ALL arm each as pairMarketQuery does,
// since a venue writing base = token_in stores one market as (A,B) and (B,A). The second arm
// excludes the first's rows (they only overlap when base and quote name the same asset).
// markets_24h counts marketKeySQL's unordered pair, inlined to keep the strings static.
const (
	pairSourceStatsQuery = `
		WITH per_source AS (
		SELECT source,
		       COUNT(*)::bigint AS trades_24h,
		       SUM(usd_volume::numeric)::text AS volume_usd_24h,
		       COUNT(DISTINCT (LEAST(base_asset, quote_asset), GREATEST(base_asset, quote_asset)))::bigint AS markets_24h,
		       COUNT(*) FILTER (WHERE usd_volume IS NULL)::bigint AS unpriced_trades_24h
		  FROM (
		    SELECT source, base_asset, quote_asset, usd_volume
		      FROM trades
		     WHERE ts >= now() - INTERVAL '24 hours'
		       AND base_asset = ANY($1) AND quote_asset = ANY($2)
		    UNION ALL
		    SELECT source, base_asset, quote_asset, usd_volume
		      FROM trades
		     WHERE ts >= now() - INTERVAL '24 hours'
		       AND base_asset = ANY($2) AND quote_asset = ANY($1)
		       AND NOT (base_asset = ANY($1) AND quote_asset = ANY($2))
		  ) t
		 GROUP BY source
		)
		SELECT source, trades_24h, volume_usd_24h, markets_24h, unpriced_trades_24h FROM per_source
		 ORDER BY volume_usd_24h::numeric DESC NULLS LAST, trades_24h DESC, source
	`
	assetSourceStatsQuery = `
		WITH per_source AS (
		SELECT source,
		       COUNT(*)::bigint AS trades_24h,
		       SUM(usd_volume::numeric)::text AS volume_usd_24h,
		       COUNT(DISTINCT (LEAST(base_asset, quote_asset), GREATEST(base_asset, quote_asset)))::bigint AS markets_24h,
		       COUNT(*) FILTER (WHERE usd_volume IS NULL)::bigint AS unpriced_trades_24h
		  FROM trades
		 WHERE ts >= now() - INTERVAL '24 hours'
		   AND (base_asset = ANY($1) OR quote_asset = ANY($1))
		 GROUP BY source
		)
		SELECT source, trades_24h, volume_usd_24h, markets_24h, unpriced_trades_24h FROM per_source
		 ORDER BY volume_usd_24h::numeric DESC NULLS LAST, trades_24h DESC, source
	`
)

// PairSourceStats returns trailing-24h per-source USD volume and trade count for one (base, quote) market, ordered by USD volume desc (unpriced last); base/quote are the full set of canonical forms to match.
func (s *Store) PairSourceStats(ctx context.Context, base, quote []string) ([]SourceStats, error) {
	return s.scanSourceStats(ctx, pairSourceStatsQuery, base, quote)
}

// AssetSourceStats returns trailing-24h per-source USD volume and trade count over every market the asset appears in (either side), in PairSourceStats' order; asset is the full set of canonical forms so multi-form assets aggregate together.
func (s *Store) AssetSourceStats(ctx context.Context, asset []string) ([]SourceStats, error) {
	return s.scanSourceStats(ctx, assetSourceStatsQuery, asset)
}

// scanSourceStats runs a static per-source query and scans rows; MarketsCount24h is meaningful only for the asset variant (1 for a
// single pair).
func (s *Store) scanSourceStats(ctx context.Context, query string, args ...any) ([]SourceStats, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("timescale: scanSourceStats: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []SourceStats
	for rows.Next() {
		var ss SourceStats
		if err := rows.Scan(
			&ss.Source,
			&ss.TradeCount24h,
			&ss.VolumeUSD24h,
			&ss.MarketsCount24h,
			&ss.UnpricedTrades24h,
		); err != nil {
			return nil, fmt.Errorf("timescale: scanSourceStats scan: %w", err)
		}
		out = append(out, ss)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: scanSourceStats rows: %w", err)
	}
	return out, nil
}
