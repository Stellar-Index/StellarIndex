package timescale

import (
	"context"
	"fmt"
	"time"

	"github.com/RatesEngine/rates-engine/internal/canonical"
)

// Market is one distinct (base, quote) pair summary with activity
// statistics. Returned by [Store.DistinctPairs].
type Market struct {
	Pair          canonical.Pair
	LastTradeAt   time.Time
	TradeCount24h int64
}

// DistinctPairs returns one page of (base, quote) pairs present in
// the trades hypertable, each with its last-trade timestamp and a
// 24h trade count. Cursor-based pagination keyed on the pair's
// canonical "<base>|<quote>" string; empty cursor starts from the
// beginning.
//
// limit clamps to [1, 500] — matching DistinctAssets for consistency.
//
// Performance: scans the trades hypertable with GROUP BY over two
// string columns. Correct but not cheap at scale. Planned
// optimisation: materialised market_catalogue populated by the
// indexer alongside the asset catalogue (TODO migrations/0004).
func (s *Store) DistinctPairs(ctx context.Context, cursor string, limit int) ([]Market, string, error) {
	if limit < 1 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}

	// The cursor is the concatenation "<base>|<quote>". We compare
	// on the same expression in SQL for stable page breaks.
	const q = `
        SELECT base_asset, quote_asset,
               MAX(ts) AS last_trade_at,
               SUM(CASE WHEN ts > NOW() - INTERVAL '24 hours' THEN 1 ELSE 0 END) AS count_24h
          FROM trades
         GROUP BY base_asset, quote_asset
        HAVING ($1 = '' OR (base_asset || '|' || quote_asset) > $1)
         ORDER BY (base_asset || '|' || quote_asset) ASC
         LIMIT $2
    `
	rows, err := s.db.QueryContext(ctx, q, cursor, limit+1)
	if err != nil {
		return nil, "", fmt.Errorf("timescale: DistinctPairs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]Market, 0, limit)
	n := 0
	var extraCursor string
	for rows.Next() {
		var (
			baseRaw, quoteRaw string
			lastAt            time.Time
			count24h          int64
		)
		if err := rows.Scan(&baseRaw, &quoteRaw, &lastAt, &count24h); err != nil {
			return nil, "", fmt.Errorf("timescale: DistinctPairs scan: %w", err)
		}
		n++
		if n > limit {
			extraCursor = baseRaw + "|" + quoteRaw
			break
		}
		base, err := canonical.ParseAsset(baseRaw)
		if err != nil {
			return nil, "", fmt.Errorf("timescale: DistinctPairs base %q: %w", baseRaw, err)
		}
		quote, err := canonical.ParseAsset(quoteRaw)
		if err != nil {
			return nil, "", fmt.Errorf("timescale: DistinctPairs quote %q: %w", quoteRaw, err)
		}
		pair, err := canonical.NewPair(base, quote)
		if err != nil {
			return nil, "", fmt.Errorf("timescale: DistinctPairs pair: %w", err)
		}
		out = append(out, Market{
			Pair:          pair,
			LastTradeAt:   lastAt.UTC(),
			TradeCount24h: count24h,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("timescale: DistinctPairs rows: %w", err)
	}

	nextCursor := ""
	if extraCursor != "" && len(out) > 0 {
		// Cursor points at the LAST row IN the returned page.
		last := out[len(out)-1]
		nextCursor = last.Pair.Base.String() + "|" + last.Pair.Quote.String()
	}
	return out, nextCursor, nil
}
