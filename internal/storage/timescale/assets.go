package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// DistinctAssets returns one page of assets that have appeared in
// the trades hypertable (as base OR quote) within the last
// [MarketsRecencyWindow] (14 days by default). Cursor-based
// pagination keyed on the asset-id string. Empty cursor starts
// from the beginning. limit is clamped to [1, 500].
//
// Returns (assets, nextCursor, err). nextCursor is empty when the
// page is the last one.
//
// Recency window: matches /v1/markets's "active assets" semantic.
// Without the window the UNIONed DISTINCT scans run across every
// chunk in the trades hypertable (539M+ rows on r1) — measured at
// 4-5 minutes per call, far past any client deadline. With the
// 14-day cap the scan touches ~1.5M rows and finishes inside the
// 30s API budget. Without it the unbounded query would run on every
// /v1/assets call; the recency cap brings the endpoint into the
// SLA range without a new materialised table. The planned
// optimisation is a materialised `asset_catalogue` populated
// incrementally by the indexer (future migration; not on main
// today) — that would let us drop the recency bound entirely.
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

// HasAsset reports whether this index knows the asset. Cheap existence
// check — doesn't page through data.
//
// Returns (true, nil) for known asset; (false, nil) for unknown;
// (_, err) for a query failure.
//
// Dispatch:
//
//   - AssetClassic: PK lookup on `classic_assets`. The registry table
//     has one row per (code, issuer) ever observed and a primary key
//     on `asset_id`; an unknown classic asset costs one index seek.
//     Bypasses the trades hypertable entirely: answering from `trades`
//     measured 4-5 s on the `/v1/assets/AAAA-G…` cold path, because the
//     `WHERE base_asset = $1 OR quote_asset = $1` across 2.7 B trades
//     rows has to seek every chunk's index.
//   - Every other type (native / soroban / fiat / crypto / rwa):
//     [Store.hasNonClassicAsset], a WINDOW-BOUNDED probe. No registry
//     table can hold these types — `classic_assets` requires a non-null
//     `issuer_g_strkey`, which native and contract assets structurally
//     do not have — so the window bound, not a registry, is what keeps
//     the read off the cold end of the hypertable.
func (s *Store) HasAsset(ctx context.Context, a canonical.Asset) (bool, error) {
	// XLM is not discovered by trading. It is the network's native asset:
	// it exists on every Stellar network from the genesis ledger, whether
	// or not anyone has traded it, so no evidence needs to be sought and
	// none can be absent. Answering it from trade activity is a category
	// error that bites on a QUIET network — when measured,
	// futurenet had ZERO XLM trades in the 14-day window this file's
	// non-classic probe bounds on, so routing native through that probe
	// would 404 the native asset of a network we ask developers to build
	// against. testnet had 2,030 in the same window and would have looked
	// fine, which is exactly how this ships unnoticed.
	if a.Type == canonical.AssetNative {
		return true, nil
	}
	if a.Type == canonical.AssetClassic {
		return s.hasClassicAsset(ctx, a)
	}
	return s.hasNonClassicAsset(ctx, a)
}

// hasClassicAsset is the fast path: PK lookup on
// classic_assets. The registry was specifically designed (migration
// 0023) as "the catalogue of every classic asset ever observed."
//
// The registry is not populated only by the trade-insert hook: since
// migration 0158 it also carries assets registered from TRUSTLINE
// HOLDINGS ([Store.RegisterClassicAssetsHeld]), which is 61% of the
// classic-asset population. Presence here is therefore NOT a subset of
// presence in `trades`, and that is the point: without the holdings rows
// an asset that is held but never traded would have no row here,
// [Store.HasAsset] would answer false for an asset that demonstrably
// exists, and GET /v1/assets/{id} would 404 it.
//
// The short-circuit is sound: this function answers "does this asset
// exist", not "has it traded". A hit avoids the hypertable; a miss means
// we have never observed the asset from any source. A hit does not imply
// a trade — read last_trade_at for that.
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

// hasNonClassicAsset answers existence for every asset type the classic
// registry cannot hold.
//
// It avoids the unbounded `trades WHERE base = $1 OR quote = $1` scan the
// no-unbounded-trade-scan rule forbids: the OR BitmapOrs every chunk back
// to 2017, and on r1 `GET /v1/assets/native` blew the 15 s budget while
// historical chunks were being recompressed. Instead three probes are
// ORed and evaluated lazily: classic_assets over the alias forms (so a
// declared SAC agrees with its classic twin), then trades base side and
// quote side, each `= ANY($1) AND ts >= $2` so each leads on its own
// column's index. `since` is computed Go-side so the planner excludes
// out-of-window chunks at plan time. The probes bind [assetAliasArray],
// so XLM's three spellings answer alike.
//
// The answer is "traded inside [MarketsRecencyWindow], or in the classic
// registry", the window [Store.DistinctAssets] uses, so the detail route
// cannot 404 an asset the listing shows. A non-classic asset whose only
// trades predate the window now answers false; for Soroban contracts that
// residual is contracts the listing does not show either. Removing it
// needs a writer-side asset-seen registry for every type.
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
