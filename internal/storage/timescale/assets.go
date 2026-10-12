package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// DistinctAssets returns one page of assets seen as base or quote within [MarketsRecencyWindow], keyed by asset-id cursor (limit clamped to [1, 500]); the window bounds the UNIONed DISTINCT scan.
func (s *Store) DistinctAssets(ctx context.Context, cursor string, limit int) ([]canonical.Asset, string, error) {
	if limit < 1 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}

	// `since` is computed Go-side rather than `NOW() - INTERVAL`
	// so the planner sees a constant timestamp parameter and prunes
	// chunks at plan time. Same trick the markets query uses.
	since := time.Now().UTC().Add(-MarketsRecencyWindow)
	const q = `
        SELECT asset FROM (
            SELECT DISTINCT base_asset  AS asset FROM trades WHERE ts >= $3
            UNION
            SELECT DISTINCT quote_asset AS asset FROM trades WHERE ts >= $3
        ) s
        WHERE ($1 = '' OR asset > $1)
        ORDER BY asset
        LIMIT $2
    `
	// We ask for one extra row to detect whether another page
	// exists — if we get (limit + 1) rows, the first `limit` are
	// the page and the last row's asset-id is the next cursor.
	rows, err := s.db.QueryContext(ctx, q, cursor, limit+1, since)
	if err != nil {
		return nil, "", fmt.Errorf("timescale: DistinctAssets: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]canonical.Asset, 0, limit)
	hasMore := false
	n := 0
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, "", fmt.Errorf("timescale: DistinctAssets scan: %w", err)
		}
		n++
		if n > limit {
			// Extra row — not returned; it only tells us another page
			// exists. The nextCursor below is still the last row IN
			// the page so the next query resumes via `asset > cursor`.
			hasMore = true
			break
		}
		parsed, perr := canonical.ParseAsset(raw)
		if perr != nil {
			return nil, "", fmt.Errorf("timescale: DistinctAssets parse %q: %w", raw, perr)
		}
		out = append(out, parsed)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("timescale: DistinctAssets rows: %w", err)
	}

	nextCursor := ""
	if hasMore && len(out) > 0 {
		nextCursor = out[len(out)-1].String()
	}
	return out, nextCursor, nil
}

// HasAsset reports whether this index knows the asset; classic assets use a PK lookup on `classic_assets` to avoid seeking every trades chunk, other types use a window-bounded probe.
func (s *Store) HasAsset(ctx context.Context, a canonical.Asset) (bool, error) {
	// Native XLM exists from genesis whether or not anyone has traded it, so answering from
	// trade activity is a category error that bites on a quiet network: a network with no XLM
	// trades in the probe window would 404 its own native asset.
	if a.Type == canonical.AssetNative {
		return true, nil
	}
	if a.Type == canonical.AssetClassic {
		return s.hasClassicAsset(ctx, a)
	}
	return s.hasNonClassicAsset(ctx, a)
}

// hasClassicAsset is the PK lookup on classic_assets. The registry also holds trustline-only assets, so it answers "exists", not "has
// traded".
func (s *Store) hasClassicAsset(ctx context.Context, a canonical.Asset) (bool, error) {
	const q = `SELECT EXISTS (SELECT 1 FROM classic_assets WHERE asset_id = $1)`
	var exists bool
	err := s.db.QueryRowContext(ctx, q, a.String()).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("timescale: hasClassicAsset: %w", err)
	}
	return exists, nil
}

// hasNonClassicAsset probes classic_assets (alias forms), then trades base and quote each as `= ANY($1) AND ts >= $2`, avoiding the
// unbounded `base = $1 OR quote = $1` BitmapOr. Window matches [Store.DistinctAssets] so detail cannot 404 a listed asset; `since` is
// Go-side for plan-time pruning.
func (s *Store) hasNonClassicAsset(ctx context.Context, a canonical.Asset) (bool, error) {
	const q = `
        SELECT
               EXISTS (SELECT 1 FROM classic_assets WHERE asset_id = ANY($1))
            OR EXISTS (SELECT 1 FROM trades
                        WHERE base_asset  = ANY($1) AND ts >= $2)
            OR EXISTS (SELECT 1 FROM trades
                        WHERE quote_asset = ANY($1) AND ts >= $2)
    `
	// Go-side, like [Store.DistinctAssets] one function up: a constant
	// timestamp parameter is what the planner prunes chunks with at plan
	// time. `now() - INTERVAL` would not be.
	since := time.Now().UTC().Add(-MarketsRecencyWindow)
	var exists bool
	err := s.db.QueryRowContext(ctx, q, assetAliasArray(a.String()), since).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("timescale: hasNonClassicAsset: %w", err)
	}
	return exists, nil
}
