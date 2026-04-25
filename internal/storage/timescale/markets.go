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
// indexer alongside the asset catalogue (see DistinctAssets's
// performance note — both rides on the same future migration).
func (s *Store) DistinctPairs(ctx context.Context, cursor string, limit int) ([]Market, string, error) {
	if limit < 1 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}

	// The cursor is the concatenation "<base>|<quote>". We filter in
	// WHERE (not HAVING) so pairs before the cursor are excluded
	// BEFORE the per-group aggregation runs — the 24h-count CASE/SUM
	// is the expensive part of this query, and with HAVING the
	// planner can't skip any trade rows for filtered-out pairs. Also
	// opens the door for an index-only scan on (base_asset,
	// quote_asset) if one ever lands.
	//
	// Overfetch by one (LIMIT $2 = limit+1) to detect "more pages
	// exist". The extra row isn't returned to the caller; its only
	// purpose is to toggle whether we emit a nextCursor.
	const q = `
        SELECT base_asset, quote_asset,
               MAX(ts) AS last_trade_at,
               SUM(CASE WHEN ts > NOW() - INTERVAL '24 hours' THEN 1 ELSE 0 END) AS count_24h
          FROM trades
         WHERE $1 = '' OR (base_asset || '|' || quote_asset) > $1
         GROUP BY base_asset, quote_asset
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
	hasMore := false
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
			hasMore = true
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
	if hasMore && len(out) > 0 {
		// Cursor points at the LAST row IN the returned page — next
		// call resumes at "strictly greater than this pair".
		last := out[len(out)-1]
		nextCursor = last.Pair.Base.String() + "|" + last.Pair.Quote.String()
	}
	return out, nextCursor, nil
}

// PairMarket returns the activity summary for a single (base, quote)
// pair. The bool result is false when the pair has no trades in the
// hypertable; callers translate that to an empty list (200 OK) per
// the /v1/pairs envelope contract — not a 404 — to match the
// "array of MarketRow" spec shape.
//
// Performance: bounded scan over the trades hypertable for one
// (base, quote) tuple. The same future market_catalogue
// materialised view that benefits DistinctPairs benefits this too.
func (s *Store) PairMarket(ctx context.Context, base, quote canonical.Asset) (Market, bool, error) {
	const q = `
        SELECT MAX(ts) AS last_trade_at,
               SUM(CASE WHEN ts > NOW() - INTERVAL '24 hours' THEN 1 ELSE 0 END) AS count_24h
          FROM trades
         WHERE base_asset = $1 AND quote_asset = $2
    `
	var (
		lastAt   *time.Time
		count24h *int64
	)
	if err := s.db.QueryRowContext(ctx, q, base.String(), quote.String()).Scan(&lastAt, &count24h); err != nil {
		return Market{}, false, fmt.Errorf("timescale: PairMarket: %w", err)
	}
	if lastAt == nil {
		return Market{}, false, nil
	}
	pair, err := canonical.NewPair(base, quote)
	if err != nil {
		return Market{}, false, fmt.Errorf("timescale: PairMarket pair: %w", err)
	}
	var n int64
	if count24h != nil {
		n = *count24h
	}
	return Market{
		Pair:          pair,
		LastTradeAt:   lastAt.UTC(),
		TradeCount24h: n,
	}, true, nil
}
