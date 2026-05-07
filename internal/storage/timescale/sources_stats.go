package timescale

import (
	"context"
	"database/sql"
	"fmt"
)

// SourceStats is the per-source 24h activity row.
type SourceStats struct {
	Source         string
	TradeCount24h  int64
	// VolumeUSD24h is SUM(usd_volume) over trades in the trailing
	// 24h. Numeric stringified so we don't lose precision crossing
	// the wire (and to match the rest of the Volume24hUSD shape).
	// "" when no trades had populated usd_volume in the window
	// (e.g. an oracle source whose decoder doesn't set usd_volume).
	VolumeUSD24h sql.NullString
	// MarketsCount24h is COUNT(DISTINCT (base_asset, quote_asset))
	// — the number of unique (base, quote) pairs the source
	// observed in the trailing 24h. A useful "pools per DEX"
	// proxy for AMMs where each pair contract = one pool.
	MarketsCount24h int64
}

// GetSourceStats returns trailing-24h trade counts grouped by source.
// Cheap aggregation against the trades hypertable; the source
// column is well-covered by the (ts, source, base_asset,
// quote_asset) ingest pattern.
//
// Sources with no trades in 24h are absent from the result —
// callers join against the static external.Registry to fill in.
func (s *Store) GetSourceStats(ctx context.Context) ([]SourceStats, error) {
	const q = `
		SELECT source,
		       COUNT(*)::bigint                                AS trades_24h,
		       SUM(usd_volume)::text                           AS volume_usd_24h,
		       COUNT(DISTINCT (base_asset, quote_asset))::bigint AS markets_24h
		  FROM trades
		 WHERE ts >= now() - INTERVAL '24 hours'
		 GROUP BY source
		 ORDER BY 2 DESC
	`
	rows, err := s.db.QueryContext(ctx, q)
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
