package timescale

import (
	"context"
	"fmt"
	"time"
)

// SorobanDEXTradeRef is one (source, asset) pair where a DEX trade for a
// Soroban-contract asset landed in the served `trades` tier. Asset is the
// token's C-strkey contract id (56 chars). Consumed by the aggregator's
// decimals-guard sweep (internal/decimalsguard), which resolves each
// asset's on-chain decimals() and alerts when it is not 7.
type SorobanDEXTradeRef struct {
	Source string
	Asset  string
}

// RecentSorobanDEXTrades returns the DISTINCT (source, Soroban-contract-asset)
// pairs whose trades landed at or after `since`.
//
// Scope: only Soroban-contract asset keys are returned — the ONLY assets that
// can carry non-7 decimals. Classic (`CODE-ISSUER`), `native`, `fiat:*` and
// `crypto:*` keys are 7-dp-or-not-a-DEX-token and are excluded in SQL:
//
//   - `>= 'C' AND < 'D'`  — contract strkeys start with 'C' (a sargable range
//     on trades_base_ts_idx / trades_quote_ts_idx, NOT a `LIKE 'C%'` scan).
//   - `char_length(...) = 56` and `position('-' in ...) = 0` — a contract
//     strkey is exactly 56 chars with no `-`; this rejects a classic credit
//     whose CODE happens to start with 'C' (e.g. `CATS-G...`).
//
// Boundedness: the caller sweeps a SHORT trailing window (minutes) on a
// periodic cadence — `since` is computed Go-side so the planner prunes chunks
// at plan time. This is deliberately NOT an unbounded DISTINCT over all
// `trades` history (the full-sort trap; see the trades-scan discipline). The
// window's distinct output is a handful of rows.
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
