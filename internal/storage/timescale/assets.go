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
// 30s API budget. Pre-2026-05-04 the unbounded query ran every
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
//     Bypasses the trades hypertable entirely. F-0157 perf
//     (audit-2026-05-26): pre-fix `/v1/assets/AAAA-G…` cold path was
//     4-5 s because the `WHERE base_asset = $1 OR quote_asset = $1`
//     across 2.7 B trades rows had to seek every chunk's index.
//   - Every other type (native / soroban / fiat / crypto / rwa):
//     [Store.hasNonClassicAsset], a WINDOW-BOUNDED probe. No registry
//     table can hold these types — `classic_assets` requires a non-null
//     `issuer_g_strkey`, which native and contract assets structurally
//     do not have — so the window bound, not a registry, is what keeps
//     the read off the cold end of the hypertable.
func (s *Store) HasAsset(ctx context.Context, a canonical.Asset) (bool, error) {
	if a.Type == canonical.AssetClassic {
		return s.hasClassicAsset(ctx, a)
	}
	return s.hasNonClassicAsset(ctx, a)
}

// hasClassicAsset is the F-0157-perf fast path: PK lookup on
// classic_assets. The registry was specifically designed (migration
// 0023) as "the catalogue of every classic asset ever observed,"
// populated by the trade-insert hook via
// `Store.registerClassicAssetSeen`. So the asset's presence in
// classic_assets is a strict subset of its presence in trades —
// which means an asset_id NOT in classic_assets has no trades
// either, and we can short-circuit without touching the hypertable.
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
// # What this replaced, and why
//
// Until 2026-09-09 this arm was an UNBOUNDED existence scan:
//
//	SELECT EXISTS (SELECT 1 FROM trades
//	                WHERE base_asset = $1 OR quote_asset = $1 LIMIT 1)
//
// — precisely the shape the standing no-unbounded-trade-scan rule
// forbids, and the same F-0157 shape hasClassicAsset was written to
// escape, left behind on the arm that serves `native`. The OR across
// two columns cannot be one scan of either single-column index, so the
// planner BitmapOrs both per chunk and appends chunks until a row turns
// up; on r1 that is thousands of chunks spanning 2017, nearly all
// compressed. It survived only by getting lucky early in the append
// order. Measured on r1 2026-09-09, while a usd-volume re-stamp was
// decompressing and re-compressing historical chunks (routine
// maintenance here), `GET /v1/assets/native` blew the 15 s request
// budget on three consecutive attempts — `GetAsset failed
// err="timescale: HasAsset: timeout: context deadline exceeded"` —
// while `/v1/assets/USDC-GA5Z…` on the classic arm answered in 2.5 ms.
// The native asset of the network this project indexes was the one
// asset the fast path did not cover.
//
// # The shape now
//
// Three index probes ORed together, short-circuited on the first TRUE:
//
//  1. `classic_assets` over the ALIAS forms. Reached only when one of
//     them is classic — a SAC whose classic twin the operator declared
//     in `[supply].sac_wrappers` — and it is what makes HasAsset(SAC)
//     agree with HasAsset(classic twin) for one and the same asset. One
//     PK seek; never matches for native/fiat/crypto/rwa.
//  2. and 3. `trades`, base side then quote side, each `= ANY($1) AND
//     ts >= $2`. Splitting the OR into two single-column predicates is
//     what lets each one lead on an index of the column it probes (the
//     shape [Store.RecentSorobanDEXTrades] already uses), and `since` is
//     computed Go-side — never `now() - INTERVAL` — so the planner sees
//     a constant timestamp and excludes out-of-window chunks at plan
//     time. That exclusion IS the fix: maintenance on historical chunks
//     is no longer on this query's path.
//
// Measured on a migrated Timescale 2.26/pg15 with 40 days of trades
// (chunk interval 7 days since migration 0062): the pre-fix statement
// plans a BitmapOr against every chunk; this one plans three InitPlans —
// a classic_assets index-only scan, then per-side index scans over the
// in-window chunks ONLY, with the `ts >= …` qual dropped on the chunks
// wholly inside the window and kept on the straddling one. Postgres
// evaluates the InitPlans lazily across the OR, so a hit on an early
// probe leaves the later ones unexecuted.
//
// Alias-complete per the XLM dual-form rule: the probes bind
// [assetAliasArray], so `native`, `crypto:XLM` and the XLM SAC each
// answer from all three spellings and no form is made fast at another's
// expense. Existence is an identity question, so folding the spellings
// here does not merge their (genuinely disjoint) venue populations —
// every read that returns DATA still keys on the form it was asked for.
//
// # The semantic this narrows, deliberately
//
// The answer is now "traded inside [MarketsRecencyWindow], or carried
// by the classic registry" rather than "traded at any point in
// history". That is the same window [Store.DistinctAssets] uses, and
// the equality is the point: `/v1/assets/{id}` now answers for a
// superset of exactly the population `/v1/assets` lists, so the detail
// route cannot 404 something the listing shows. The old semantic was
// never reachable inside a request budget — it required the unbounded
// scan above — so this narrows a promise the code could not keep.
//
// It is NOT a superset of "ever traded": a non-classic asset whose only
// trades predate the window now answers false where the old scan would
// eventually have answered true. For native/fiat/crypto/rwa that
// population is empty on a live network (those forms trade continuously
// if they trade at all); for Soroban contracts it is bounded by the
// listing's own 24h-volume gate, i.e. contracts the listing does not
// show either. The durable removal of that residual is a general
// asset-seen registry: `registerClassicAssetSeen` early-returns for
// non-classic assets today, so extending the trade-insert hook (plus a
// chunked backfill) would give every type the strict-superset argument
// hasClassicAsset gets. That is a writer-side change with a migration,
// filed as follow-up rather than smuggled into a read-path fix.
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
