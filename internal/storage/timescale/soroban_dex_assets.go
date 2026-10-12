package timescale

import (
	"context"
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
	const q = `
SELECT DISTINCT source, base_asset AS asset
  FROM trades
 WHERE base_asset >= 'C' AND base_asset < 'D'
   AND char_length(base_asset) = 56
   AND position('-' in base_asset) = 0
   AND ts >= $1
UNION
SELECT DISTINCT source, quote_asset AS asset
  FROM trades
 WHERE quote_asset >= 'C' AND quote_asset < 'D'
   AND char_length(quote_asset) = 56
   AND position('-' in quote_asset) = 0
   AND ts >= $1`

	rows, err := s.db.QueryContext(ctx, q, since.UTC())
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
