package timescale

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// SorobanDEXTradeRef is one (source, asset) pair where a DEX trade for a Soroban-contract asset landed in the served trades tier; Asset is the token's 56-char C-strkey.
type SorobanDEXTradeRef struct {
	Source string
	Asset  string
}

// RecentSorobanDEXTrades returns the DISTINCT (source, Soroban-contract-asset) pairs traded at or after since; only Soroban-contract keys, the only assets that can carry non-7 decimals.
func (s *Store) RecentSorobanDEXTrades(ctx context.Context, since time.Time) ([]SorobanDEXTradeRef, error) {
	// pools_per_source_1h holds the same (source, base, quote) keys as trades, one row per hour
	// instead of per trade, so it answers up to its last materialized bucket and raw trades
	// answer from there. The watermark is a bound parameter: as a join it defeats chunk exclusion.
	var watermark sql.NullTime
	if err := s.db.QueryRowContext(ctx, `SELECT max(bucket) FROM pools_per_source_1h`).Scan(&watermark); err != nil {
		return nil, fmt.Errorf("timescale: RecentSorobanDEXTrades watermark: %w", err)
	}

	// A NULL watermark (nothing materialized) empties the cagg branches and greatest() ignores it.
	const q = `
SELECT source, base_asset AS asset FROM pools_per_source_1h
 WHERE bucket >= time_bucket('1 hour', $1::timestamptz) AND bucket < $2
   AND base_asset >= 'C' AND base_asset < 'D'
   AND char_length(base_asset) = 56
   AND position('-' in base_asset) = 0
UNION
SELECT source, quote_asset FROM pools_per_source_1h
 WHERE bucket >= time_bucket('1 hour', $1::timestamptz) AND bucket < $2
   AND quote_asset >= 'C' AND quote_asset < 'D'
   AND char_length(quote_asset) = 56
   AND position('-' in quote_asset) = 0
UNION
SELECT source, base_asset FROM trades
 WHERE ts >= greatest($1::timestamptz, $2::timestamptz)
   AND base_asset >= 'C' AND base_asset < 'D'
   AND char_length(base_asset) = 56
   AND position('-' in base_asset) = 0
UNION
SELECT source, quote_asset FROM trades
 WHERE ts >= greatest($1::timestamptz, $2::timestamptz)
   AND quote_asset >= 'C' AND quote_asset < 'D'
   AND char_length(quote_asset) = 56
   AND position('-' in quote_asset) = 0`

	rows, err := s.db.QueryContext(ctx, q, since.UTC(), watermark)
	if err != nil {
		return nil, fmt.Errorf("timescale: RecentSorobanDEXTrades: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []SorobanDEXTradeRef
	for rows.Next() {
		var ref SorobanDEXTradeRef
		if err := rows.Scan(&ref.Source, &ref.Asset); err != nil {
			return nil, fmt.Errorf("timescale: RecentSorobanDEXTrades scan: %w", err)
		}
		out = append(out, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: RecentSorobanDEXTrades rows: %w", err)
	}
	return out, nil
}
