package timescale

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/lib/pq"
)

// AssetRow is the read-side projection of one row from the
// asset-discovery view: classic_assets joined with whatever supply +
// activity counters we have today. Pure-string fields keep the
// surface decoupled from the canonical types package.
//
// VWAP / Volume24hUSD / MarketCapUSD are nullable when the
// aggregator hasn't yet computed values for the asset — newly-
// observed assets, illiquid tokens with no off-chain peg, etc.
// Pointer types let the wire layer emit `null` cleanly.
type AssetRow struct {
	Slug             string
	AssetID          string
	Code             string
	IssuerGStrkey    string
	FirstSeenLedger  uint32
	LastSeenLedger   uint32
	ObservationCount int64

	// Latest VWAP-against-USD if available. Nil when:
	//   - no off-chain peg (illiquid Soroban-only token), or
	//   - the asset has no `fiat:USD` quote pair, or
	//   - the freeze writer has frozen this pair.
	PriceUSD *string
	// Trailing-24h USD-denominated trade volume. Nil when no
	// trades hit `usd_volume`-eligible quotes in 24h.
	Volume24hUSD *string
	// Market cap in USD = price × circulating_supply when both
	// are known. Nil when either component is missing.
	MarketCapUSD *string
	// Circulating supply in canonical units (decimal string;
	// scale matches asset decimals). Nil for assets the supply
	// pipeline doesn't yet cover.
	CirculatingSupply *string
	// Trailing-1h / 24h / 7d price change as a signed percentage
	// with two fractional digits (e.g. "+1.27", "-0.05", "0.00").
	// Nil when the asset has no current price, or when no
	// matching past-bucket exists in prices_1m within the
	// window-specific tolerance (±5min for 1h, ±30min for 24h,
	// ±2h for 7d).
	Change1hPct  *string
	Change24hPct *string
	Change7dPct  *string
}

// AssetsOrder controls the sort + cursor scheme used by ListAssets.
// Default ObservationCountDesc preserves the original "rank by
// activity" semantics; Volume24hUSDDesc is the volume-first view
// the explorer's /assets table can opt into for live-volume
// rankings.
type AssetsOrder int

const (
	// AssetsOrderObservationCountDesc orders by all-time observation
	// count desc (a cheap activity proxy). Cursor is
	// `<obs_count>:<asset_id>`.
	AssetsOrderObservationCountDesc AssetsOrder = iota
	// AssetsOrderVolume24hUSDDesc orders by trailing-24h USD volume
	// desc (NULLS LAST), with `<asset_id>` as the tie-breaker.
	// Cursor is `<vol_or_blank>:<asset_id>`.
	AssetsOrderVolume24hUSDDesc
)

// ListAssetsOptions bundles the optional filters / paging
// parameters for ListAssets. Zero values are the API defaults.
type ListAssetsOptions struct {
	// Limit clamps to [1, 500]; 0 → 100.
	Limit int
	// Issuer, when non-empty, restricts to that G-strkey.
	Issuer string
	// Code, when non-empty, restricts to rows whose classic asset
	// code matches EXACTLY (case-sensitive — Stellar codes are
	// case-significant; USDC and usdc are distinct assets). Codes
	// are not unique on Stellar, so combine with Issuer to pin a
	// single asset. Pushes down to the indexed classic_assets.code
	// column (BACKLOG #54).
	Code string
	// Cursor is the keyset cursor returned by the previous
	// response's NextCursor field. Empty for the first page.
	Cursor string
	// Q, when non-empty, filters rows where code, slug, or
	// issuer_g_strkey contains the substring (case-insensitive).
	// Useful for the explorer's `/assets?q=…` search box —
	// otherwise a 440K-asset directory is unsearchable.
	Q string
	// Order controls the sort + cursor scheme. Zero value is
	// observation_count desc (preserves the historical contract).
	Order AssetsOrder
}

// ListAssets returns asset-directory rows ordered by observation
// count desc (a cheap proxy for activity).
//
// Pagination uses a keyset cursor: the cursor encodes the
// (observation_count, asset_id) tuple of the last row from the
// previous page. Empty cursor means "first page". Cursor format:
// `<observation_count>:<asset_id>`.
func (s *Store) ListAssets(ctx context.Context, limit int, issuer, cursor string) ([]AssetRow, error) {
	return s.ListAssetsExt(ctx, ListAssetsOptions{Limit: limit, Issuer: issuer, Cursor: cursor})
}

// LatestCirculatingSupply returns the most-recent circulating supply per
// canonical asset_id from the supply_1d CAGG — the set the three-domain
// supply pipeline currently tracks (the major classic + native assets).
// Keys are canonical asset_ids: supply_1d's asset_key is CODE:ISSUER
// (colon) and `XLM`, which we translate to the listing's CODE-ISSUER
// (dash) and `native`. The /v1/assets listing uses this to fill
// market_cap WHERE supply exists rather than leaving every row null;
// coverage grows as the supply pipeline expands (it's small today).
func (s *Store) LatestCirculatingSupply(ctx context.Context) (map[string]string, error) {
	const q = `
        SELECT CASE WHEN asset_key = 'XLM' THEN 'native'
                    ELSE replace(asset_key, ':', '-') END AS asset_id,
               circulating_supply::text
          FROM supply_1d s1
         WHERE circulating_supply IS NOT NULL
           AND bucket = (SELECT max(bucket) FROM supply_1d s2 WHERE s2.asset_key = s1.asset_key)
    `
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("timescale: LatestCirculatingSupply: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]string)
	for rows.Next() {
		var assetID, circ string
		if err := rows.Scan(&assetID, &circ); err != nil {
			return nil, fmt.Errorf("timescale: LatestCirculatingSupply scan: %w", err)
		}
		out[assetID] = circ
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: LatestCirculatingSupply rows: %w", err)
	}
	return out, nil
}

// ListAssetsExt is ListAssets with the full options bag. ListAssets
// is preserved as the legacy 3-arg call so existing callers
// (handler, integration tests) compile unchanged; new callers
// pass ListAssetsOptions to opt into Q.
func (s *Store) ListAssetsExt(ctx context.Context, opts ListAssetsOptions) ([]AssetRow, error) {
	// Clamp to the documented page size, allowing one extra row for the
	// caller's overfetch-by-one pagination sentinel (501, not 500).
	// F-1326/G3-03: the previous `> 500 → 100` reset silently truncated
	// a 501-row overfetch back to 100 and dropped the cursor — clamp to
	// the ceiling instead of resetting to the default.
	limit := opts.Limit
	switch {
	case limit <= 0:
		limit = 100
	case limit > 501:
		limit = 501
	}
	query, args := buildAssetsQuery(limit, opts.Issuer, opts.Code, opts.Cursor, opts.Q, opts.Order)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("timescale: ListAssets: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]AssetRow, 0, limit)
	for rows.Next() {
		r, err := scanAssetRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: ListAssets rows: %w", err)
	}
	return out, nil
}

// scanAssetRow is the row-projection shared by ListAssetsExt and
// GetAssetBySlug. The two queries return the same column shape, so
// the scan + nullable-string unpack lives in one place. Pulled
// out of the listing loop so ListAssetsExt stays under the gocognit
// threshold as the wire shape grows.
func scanAssetRow(scanner interface {
	Scan(dest ...any) error
},
) (AssetRow, error) {
	var r AssetRow
	var (
		firstLedger, lastLedger int64
		priceUSD                sql.NullString
		volume24hUSD            sql.NullString
		marketCapUSD            sql.NullString
		circulatingSupply       sql.NullString
		change1hPct             sql.NullString
		change24hPct            sql.NullString
		change7dPct             sql.NullString
	)
	if err := scanner.Scan(
		&r.Slug, &r.AssetID, &r.Code, &r.IssuerGStrkey,
		&firstLedger, &lastLedger, &r.ObservationCount,
		&priceUSD, &volume24hUSD, &marketCapUSD, &circulatingSupply,
		&change1hPct, &change24hPct, &change7dPct,
	); err != nil {
		return AssetRow{}, fmt.Errorf("timescale: scan asset: %w", err)
	}
	r.FirstSeenLedger = uint32(firstLedger) //nolint:gosec
	r.LastSeenLedger = uint32(lastLedger)   //nolint:gosec
	r.PriceUSD = nullStringPtr(priceUSD)
	r.Volume24hUSD = nullStringPtr(volume24hUSD)
	r.MarketCapUSD = nullStringPtr(marketCapUSD)
	r.CirculatingSupply = nullStringPtr(circulatingSupply)
	r.Change1hPct = nullStringPtr(change1hPct)
	r.Change24hPct = nullStringPtr(change24hPct)
	r.Change7dPct = nullStringPtr(change7dPct)
	return r, nil
}

func nullStringPtr(ns sql.NullString) *string {
	if !ns.Valid {
		return nil
	}
	s := ns.String
	return &s
}

// listAssetsBaseSelect is the CTE-laden SELECT shared by every
// permutation of WHERE-clause buildAssetsQuery composes. Pulled
// out of the function body so buildAssetsQuery stays under the
// funlen threshold and the SQL is editable as a single block.
//
// Volume aggregation: prices_1m.volume_usd summed across the
// trailing 24h, where the asset participates as base OR quote.
// classic_asset_stats_5m is unwritten today (migration shipped
// without a writer); most classic assets have no direct
// fiat:USD pair either. The CTE-with-UNION sidesteps both.
//
// Price + 24h change: latest + 24h-ago snapshots, with XLM
// triangulation when the direct asset/fiat:USD pair doesn't
// exist. DISTINCT ON gives one "latest per asset" row without a
// window function. ±30min tolerance on 24h-ago so sparse-trade
// assets still produce a change %. Change is computed as
// (latest / ago - 1) * 100 to two fractional digits; NULL when
// either side is missing.
//
// market_cap_usd + circulating_supply remain NULL — their proper
// sources (asset_supply_history) aren't running for the long
// tail of classic assets today, and fabricating values would
// defeat the "stop lying" rule.
const listAssetsBaseSelect = `
		WITH per_asset_24h_vol AS (
		  -- #43 (2026-07-06 latency incident): read the trailing-24h
		  -- per-asset USD volume from the asset_volume_24h rollup
		  -- (migration 0087) instead of re-summing prices_1m per
		  -- request. The aggregator's assetvolrollup worker runs the
		  -- exact base-OR-quote SUM this CTE used to inline; the listing
		  -- now LEFT JOINs a small keyed-on-PK table (one row per asset
		  -- with 24h volume), so the UNFILTERED /v1/assets page stops
		  -- paying the ~4.8s cold ~256k-row scan the incident measured.
		  -- vol_usd stays NUMERIC → the wire string is byte-identical to
		  -- the old inline SUM (the rollup moved the compute, not the
		  -- value). No asset-filter pushdown here: reading the rollup is
		  -- cheap regardless of the caller's asset filter.
		  SELECT asset_id, vol_usd
		    FROM asset_volume_24h
		),
		direct_usd AS (
		  SELECT DISTINCT ON (base_asset) base_asset AS asset_id, vwap
		    FROM prices_1m
		   WHERE quote_asset = 'fiat:USD'
		     AND bucket >= now() - INTERVAL '7 days'
		     AND vwap IS NOT NULL /*PUSHDOWN_BASE*/
		   ORDER BY base_asset, bucket DESC
		),
		direct_usd_1h AS (
		  SELECT DISTINCT ON (base_asset) base_asset AS asset_id, vwap
		    FROM prices_1m
		   WHERE quote_asset = 'fiat:USD'
		     AND bucket BETWEEN now() - INTERVAL '90 minutes'
		                   AND now() - INTERVAL '55 minutes'
		     AND vwap IS NOT NULL /*PUSHDOWN_BASE*/
		   ORDER BY base_asset, bucket DESC
		),
		direct_usd_24h AS (
		  SELECT DISTINCT ON (base_asset) base_asset AS asset_id, vwap
		    FROM prices_1m
		   WHERE quote_asset = 'fiat:USD'
		     AND bucket BETWEEN now() - INTERVAL '26 hours'
		                   AND now() - INTERVAL '23 hours 30 minutes'
		     AND vwap IS NOT NULL /*PUSHDOWN_BASE*/
		   ORDER BY base_asset, bucket DESC
		),
		direct_usd_7d AS (
		  SELECT DISTINCT ON (base_asset) base_asset AS asset_id, vwap
		    FROM prices_1m
		   WHERE quote_asset = 'fiat:USD'
		     AND bucket BETWEEN now() - INTERVAL '7 days 12 hours'
		                   AND now() - INTERVAL '6 days 22 hours'
		     AND vwap IS NOT NULL /*PUSHDOWN_BASE*/
		   ORDER BY base_asset, bucket DESC
		),
		asset_vs_xlm AS (
		  SELECT DISTINCT ON (base_asset) base_asset AS asset_id, vwap
		    FROM prices_1m
		   WHERE quote_asset = 'native'
		     AND bucket >= now() - INTERVAL '7 days'
		     AND vwap IS NOT NULL /*PUSHDOWN_BASE*/
		   ORDER BY base_asset, bucket DESC
		),
		asset_vs_xlm_1h AS (
		  SELECT DISTINCT ON (base_asset) base_asset AS asset_id, vwap
		    FROM prices_1m
		   WHERE quote_asset = 'native'
		     AND bucket BETWEEN now() - INTERVAL '90 minutes'
		                   AND now() - INTERVAL '55 minutes'
		     AND vwap IS NOT NULL /*PUSHDOWN_BASE*/
		   ORDER BY base_asset, bucket DESC
		),
		asset_vs_xlm_24h AS (
		  SELECT DISTINCT ON (base_asset) base_asset AS asset_id, vwap
		    FROM prices_1m
		   WHERE quote_asset = 'native'
		     AND bucket BETWEEN now() - INTERVAL '26 hours'
		                   AND now() - INTERVAL '23 hours 30 minutes'
		     AND vwap IS NOT NULL /*PUSHDOWN_BASE*/
		   ORDER BY base_asset, bucket DESC
		),
		asset_vs_xlm_7d AS (
		  SELECT DISTINCT ON (base_asset) base_asset AS asset_id, vwap
		    FROM prices_1m
		   WHERE quote_asset = 'native'
		     AND bucket BETWEEN now() - INTERVAL '7 days 12 hours'
		                   AND now() - INTERVAL '6 days 22 hours'
		     AND vwap IS NOT NULL /*PUSHDOWN_BASE*/
		   ORDER BY base_asset, bucket DESC
		),
		xlm_usd AS (
		  -- prices_1m doesn't carry (native, fiat:USD) rows — XLM's
		  -- USD price is computed by the aggregator's triangulation
		  -- worker and lives in Redis, not the materialised view.
		  -- Mirror the aggregator's stablecoin-proxy policy in SQL
		  -- (CLAUDE.md: "stablecoin fiat-proxy is aggregator policy"
		  -- — USDC ≈ USD): use the latest on-chain XLM/USDC vwap as
		  -- the XLM/USD price. Circle's USDC issuer G-strkey is
		  -- hardcoded; a future revision pulls the list from
		  -- [trades].usd_pegged_classic_assets.
		  --
		  -- The 24h floor on bucket is REQUIRED, not just an
		  -- optimisation. With no time predicate TimescaleDB cannot
		  -- chunk-prune, so ORDER BY bucket DESC LIMIT 1 across the
		  -- 3 quote_assets must consider EVERY prices_1m chunk
		  -- (thousands post-backfill). Warm + idle that is ~13ms,
		  -- but the all-chunks access pattern degrades badly under
		  -- concurrent load + cold buffers -- observed ~40s in
		  -- pg_stat_activity during /v1/assets/{id} fan-out, the
		  -- dominant tax on every native to USD price path
		  -- (this query is #18 == #21). Bounded to 24h it touches
		  -- ~1 day of chunks (~2-3ms) and stays resilient under
		  -- load. It is also MORE correct: the unbounded form could
		  -- surface a days-stale vwap as the *current* price.
		  -- XLM/USDC is among the highest-volume pairs (trades
		  -- every minute) so a 24h floor never realistically misses
		  -- the latest. Mirrors the already-bounded
		  -- sources_stats.go xlm_usd CTE.
		  SELECT vwap
		    FROM prices_1m
		   WHERE base_asset = 'native'
		     AND quote_asset IN (
		       'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
		       'fiat:USD'
		     )
		     AND vwap IS NOT NULL
		     AND bucket >= now() - INTERVAL '24 hours'
		   ORDER BY bucket DESC
		   LIMIT 1
		),
		xlm_usd_1h AS (
		  -- 1h-ago XLM/USD via the same stablecoin-proxy policy.
		  SELECT vwap
		    FROM prices_1m
		   WHERE base_asset = 'native'
		     AND quote_asset IN (
		       'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
		       'fiat:USD'
		     )
		     AND bucket BETWEEN now() - INTERVAL '90 minutes'
		                   AND now() - INTERVAL '55 minutes'
		     AND vwap IS NOT NULL
		   ORDER BY bucket DESC
		   LIMIT 1
		),
		xlm_usd_24h AS (
		  -- 24h-ago XLM/USD via the same stablecoin-proxy policy
		  -- as xlm_usd above.
		  SELECT vwap
		    FROM prices_1m
		   WHERE base_asset = 'native'
		     AND quote_asset IN (
		       'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
		       'fiat:USD'
		     )
		     AND bucket BETWEEN now() - INTERVAL '26 hours'
		                   AND now() - INTERVAL '23 hours 30 minutes'
		     AND vwap IS NOT NULL
		   ORDER BY bucket DESC
		   LIMIT 1
		),
		xlm_usd_7d AS (
		  -- 7d-ago XLM/USD via the same stablecoin-proxy policy.
		  SELECT vwap
		    FROM prices_1m
		   WHERE base_asset = 'native'
		     AND quote_asset IN (
		       'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
		       'fiat:USD'
		     )
		     AND bucket BETWEEN now() - INTERVAL '7 days 12 hours'
		                   AND now() - INTERVAL '6 days 22 hours'
		     AND vwap IS NOT NULL
		   ORDER BY bucket DESC
		   LIMIT 1
		)
		SELECT
		    COALESCE(ca.slug, ca.code)            AS slug,
		    ca.asset_id,
		    ca.code,
		    ca.issuer_g_strkey,
		    ca.first_seen_ledger,
		    ca.last_seen_ledger,
		    ca.observation_count,
		    -- Round to 10 dp on the wire — NUMERIC * NUMERIC
		    -- preserves full precision (36+ digits) which is just
		    -- noise for a display value. 10 dp covers sub-millicent
		    -- precision (1e-10) which is finer than any asset's
		    -- meaningful tick size.
		    -- XLM itself (asset_id='native') has no rows in
		    -- direct_usd or asset_vs_xlm — its base_asset is 'native'
		    -- but neither (native, fiat:USD) nor (native, native)
		    -- exists in prices_1m. Use the xlm_usd CTE directly.
		    ROUND(COALESCE(
		      CASE WHEN ca.asset_id = 'native'
		           THEN (SELECT vwap FROM xlm_usd)
		           ELSE NULL
		      END,
		      direct.vwap,
		      vs_xlm.vwap * (SELECT vwap FROM xlm_usd)
		    ), 10)::text                          AS price_usd,
		    vol.vol_usd                           AS volume_24h_usd,
		    NULL::numeric                         AS market_cap_usd,
		    NULL::numeric                         AS circulating_supply,
		    CASE
		      WHEN ca.asset_id = 'native'
		           AND (SELECT vwap FROM xlm_usd) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_1h) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_1h) > 0
		      THEN to_char(((SELECT vwap FROM xlm_usd)
		                  / (SELECT vwap FROM xlm_usd_1h) - 1) * 100,
		                  'FM999999990.00')
		      WHEN direct.vwap IS NOT NULL AND direct_1h.vwap IS NOT NULL
		           AND direct_1h.vwap > 0
		      THEN to_char((direct.vwap / direct_1h.vwap - 1) * 100, 'FM999999990.00')
		      WHEN vs_xlm.vwap IS NOT NULL AND vs_xlm_1h.vwap IS NOT NULL
		           AND vs_xlm_1h.vwap > 0
		           AND (SELECT vwap FROM xlm_usd) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_1h) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_1h) > 0
		      THEN to_char(((vs_xlm.vwap    * (SELECT vwap FROM xlm_usd))
		                  / (vs_xlm_1h.vwap * (SELECT vwap FROM xlm_usd_1h))
		                  - 1) * 100, 'FM999999990.00')
		      ELSE NULL
		    END                                   AS change_1h_pct,
		    CASE
		      WHEN ca.asset_id = 'native'
		           AND (SELECT vwap FROM xlm_usd) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_24h) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_24h) > 0
		      THEN to_char(((SELECT vwap FROM xlm_usd)
		                  / (SELECT vwap FROM xlm_usd_24h) - 1) * 100,
		                  'FM999999990.00')
		      WHEN direct.vwap IS NOT NULL AND direct_24h.vwap IS NOT NULL
		           AND direct_24h.vwap > 0
		      THEN to_char((direct.vwap / direct_24h.vwap - 1) * 100, 'FM999999990.00')
		      WHEN vs_xlm.vwap IS NOT NULL AND vs_xlm_24h.vwap IS NOT NULL
		           AND vs_xlm_24h.vwap > 0
		           AND (SELECT vwap FROM xlm_usd) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_24h) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_24h) > 0
		      THEN to_char(((vs_xlm.vwap     * (SELECT vwap FROM xlm_usd))
		                  / (vs_xlm_24h.vwap * (SELECT vwap FROM xlm_usd_24h))
		                  - 1) * 100, 'FM999999990.00')
		      ELSE NULL
		    END                                   AS change_24h_pct,
		    CASE
		      WHEN ca.asset_id = 'native'
		           AND (SELECT vwap FROM xlm_usd) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_7d) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_7d) > 0
		      THEN to_char(((SELECT vwap FROM xlm_usd)
		                  / (SELECT vwap FROM xlm_usd_7d) - 1) * 100,
		                  'FM999999990.00')
		      WHEN direct.vwap IS NOT NULL AND direct_7d.vwap IS NOT NULL
		           AND direct_7d.vwap > 0
		      THEN to_char((direct.vwap / direct_7d.vwap - 1) * 100, 'FM999999990.00')
		      WHEN vs_xlm.vwap IS NOT NULL AND vs_xlm_7d.vwap IS NOT NULL
		           AND vs_xlm_7d.vwap > 0
		           AND (SELECT vwap FROM xlm_usd) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_7d) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_7d) > 0
		      THEN to_char(((vs_xlm.vwap    * (SELECT vwap FROM xlm_usd))
		                  / (vs_xlm_7d.vwap * (SELECT vwap FROM xlm_usd_7d))
		                  - 1) * 100, 'FM999999990.00')
		      ELSE NULL
		    END                                   AS change_7d_pct
		  FROM classic_assets ca
		  LEFT JOIN per_asset_24h_vol vol         ON vol.asset_id        = ca.asset_id
		  LEFT JOIN direct_usd        direct      ON direct.asset_id     = ca.asset_id
		  LEFT JOIN direct_usd_1h     direct_1h   ON direct_1h.asset_id  = ca.asset_id
		  LEFT JOIN direct_usd_24h    direct_24h  ON direct_24h.asset_id = ca.asset_id
		  LEFT JOIN direct_usd_7d     direct_7d   ON direct_7d.asset_id  = ca.asset_id
		  LEFT JOIN asset_vs_xlm      vs_xlm      ON vs_xlm.asset_id     = ca.asset_id
		  LEFT JOIN asset_vs_xlm_1h   vs_xlm_1h   ON vs_xlm_1h.asset_id  = ca.asset_id
		  LEFT JOIN asset_vs_xlm_24h  vs_xlm_24h  ON vs_xlm_24h.asset_id = ca.asset_id
		  LEFT JOIN asset_vs_xlm_7d   vs_xlm_7d   ON vs_xlm_7d.asset_id  = ca.asset_id
`

// listAssetsBaseSelectSQL renders [listAssetsBaseSelect] with optional
// asset-filter pushdown into each per-asset CTE (#27). When
// pushdownPredicate is empty, the function strips the
// /*PUSHDOWN_BASE*/ + /*PUSHDOWN_QUOTE*/ marker comments and
// returns the SQL unchanged (each CTE materialises stats for
// every asset). When non-empty, it prepends a `chosen_assets`
// CTE that holds the asset_id list matching pushdownPredicate
// (which the caller must construct using positional args that
// match the rest of the query) and replaces each marker with
// `AND <side>_asset IN (SELECT asset_id FROM chosen_assets)`.
//
// Measured impact (2026-05-20 on r1, /v1/assets?issuer=GA5Z…):
// each per-asset price CTE without pushdown reads 256k rows
// for a 1-row result (1.3M buffer hits); with pushdown it reads
// ~9 rows. Applies to the eight direct_usd* / asset_vs_xlm* CTEs.
// per_asset_24h_vol no longer takes part: since #43 it reads the
// asset_volume_24h rollup (migration 0087), which is a small
// keyed-on-PK table regardless of the caller's filter, so it carries
// no PUSHDOWN markers. The four xlm_usd CTEs deliberately stay
// unfiltered — they look up XLM specifically, not the caller-supplied
// asset.
//
// pushdownPredicate is the WHERE-expression body for the
// chosen_assets CTE — e.g. "issuer_g_strkey = $1" or any
// combination using positional args $N matching the caller's
// args slice. buildAssetsQuery owns the arg-numbering contract
// (issuer is $1 when set, so the predicate is the literal
// string "issuer_g_strkey = $1").
func listAssetsBaseSelectSQL(pushdownPredicate string) string {
	if pushdownPredicate == "" {
		// No pushdown — strip the marker comments. Leaving them
		// in is harmless (they're valid SQL comments) but stripping
		// keeps EXPLAIN output and pg_stat_statements normalised.
		s := strings.ReplaceAll(listAssetsBaseSelect, "/*PUSHDOWN_BASE*/", "")
		s = strings.ReplaceAll(s, "/*PUSHDOWN_QUOTE*/", "")
		return s
	}
	chosenCTE := "\n\t\tWITH chosen_assets AS (SELECT asset_id FROM classic_assets WHERE " + pushdownPredicate + "),\n\t\t"
	// Replace the leading `WITH per_asset_24h_vol AS (` with the
	// chosen_assets CTE followed by per_asset_24h_vol (chosen first
	// so its asset_id pool is materialised once and reused by every
	// downstream CTE). The original const starts with
	// "\n\t\tWITH per_asset_24h_vol AS (" — match on the WITH so the
	// replacement is unambiguous.
	s := strings.Replace(
		listAssetsBaseSelect,
		"\n\t\tWITH per_asset_24h_vol AS (",
		chosenCTE+"per_asset_24h_vol AS (",
		1,
	)
	s = strings.ReplaceAll(s, "/*PUSHDOWN_BASE*/", "AND base_asset IN (SELECT asset_id FROM chosen_assets)")
	s = strings.ReplaceAll(s, "/*PUSHDOWN_QUOTE*/", "AND quote_asset IN (SELECT asset_id FROM chosen_assets)")
	return s
}

// refreshAssetVolumeUpsert recomputes the trailing-24h per-asset USD
// volume (single-sided: the asset as base OR quote) and upserts one row
// per asset into asset_volume_24h. This is the exact SUM the
// per_asset_24h_vol CTE used to inline per request (minus the pushdown
// markers, which don't apply to a full recompute), so the rollup value
// is byte-identical to the old live figure — only NUMERIC, never float
// (ADR-0003). computed_at is stamped to the transaction timestamp so
// the sibling prune can drop assets whose volume lapsed this pass.
const refreshAssetVolumeUpsert = `
INSERT INTO asset_volume_24h AS t (asset_id, vol_usd, computed_at)
SELECT asset_id, SUM(volume_usd) AS vol_usd, now()
  FROM (
    SELECT base_asset  AS asset_id, volume_usd
      FROM prices_1m
     WHERE bucket >= now() - INTERVAL '24 hours'
       AND bucket  <  now()
       AND volume_usd IS NOT NULL
    UNION ALL
    SELECT quote_asset AS asset_id, volume_usd
      FROM prices_1m
     WHERE bucket >= now() - INTERVAL '24 hours'
       AND bucket  <  now()
       AND volume_usd IS NOT NULL
  ) t
 GROUP BY asset_id
ON CONFLICT (asset_id) DO UPDATE
   SET vol_usd     = EXCLUDED.vol_usd,
       computed_at = EXCLUDED.computed_at`

// refreshAssetVolumePrune deletes assets that fell out of the
// trailing-24h window (no prices_1m volume counted them this pass, so
// their computed_at stayed at the previous run). Same one-transaction
// now() trick as the protocol-events rollup: just-upserted rows carry
// computed_at = now() and survive; stale rows carry an older timestamp
// and are dropped.
const refreshAssetVolumePrune = `DELETE FROM asset_volume_24h WHERE computed_at < now()`

// RefreshAssetVolume24h recomputes the asset_volume_24h rollup from the
// live trailing-24h prices_1m sum and atomically replaces its contents.
// Called on a slow cadence by the aggregator's assetvolrollup worker so
// the /v1/assets listing (per_asset_24h_vol CTE) never runs the
// all-asset ~256k-row SUM per request (2026-07-06 latency incident, #43).
//
// Upsert + prune run in one transaction (row-level locks only — no
// ACCESS EXCLUSIVE table lock that would stall concurrent listing reads
// on the customer-facing endpoint).
func (s *Store) RefreshAssetVolume24h(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("timescale: RefreshAssetVolume24h begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, refreshAssetVolumeUpsert); err != nil {
		return fmt.Errorf("timescale: RefreshAssetVolume24h upsert: %w", err)
	}
	if _, err := tx.ExecContext(ctx, refreshAssetVolumePrune); err != nil {
		return fmt.Errorf("timescale: RefreshAssetVolume24h prune: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("timescale: RefreshAssetVolume24h commit: %w", err)
	}
	return nil
}

// buildAssetsQuery composes the WHERE + ORDER + LIMIT around
// listAssetsBaseSelectSQL, given the limit / issuer-filter /
// code-filter / keyset cursor / search query. The combinatorial
// explosion of (issuer × code × cursor × q) is too painful as a
// switch; use a slice + numbered placeholders.
func buildAssetsQuery(limit int, issuer, code, cursor, q string, order AssetsOrder) (string, []any) {
	var (
		conds         []string
		args          []any
		pushdownConds []string
	)
	if issuer != "" {
		args = append(args, issuer)
		conds = append(conds, fmt.Sprintf("ca.issuer_g_strkey = $%d", len(args)))
		// #27 pushdown: the issuer filter narrows the asset set
		// drastically (a single G-strkey typically issues handfuls
		// of assets, not thousands). chosen_assets materialises that
		// set once and every per-asset CTE filters against it,
		// dropping per_asset_24h_vol's row count from ~256k to ~9
		// in the GA5Z… case measured 2026-05-20. Reuses $1 (the
		// issuer arg) so no additional placeholder is needed.
		pushdownConds = append(pushdownConds, fmt.Sprintf("issuer_g_strkey = $%d", len(args)))
	}
	if code != "" {
		// BACKLOG #54: exact, case-sensitive code equality on the
		// indexed classic_assets.code column (classic_assets_code_idx).
		// A bare code is not unique (many issuers mint "USDC"), so it
		// only narrows to a handful of rows — but combined with issuer
		// it pins a single asset. Same chosen_assets pushdown as issuer:
		// reuses the outer-WHERE placeholder so no extra arg is bound.
		args = append(args, code)
		conds = append(conds, fmt.Sprintf("ca.code = $%d", len(args)))
		pushdownConds = append(pushdownConds, fmt.Sprintf("code = $%d", len(args)))
	}
	// The chosen_assets pushdown predicate ANDs whatever narrowing
	// filters are active (issuer, code, or both). Empty when neither
	// is set — listAssetsBaseSelectSQL then skips the CTE entirely.
	pushdownPredicate := strings.Join(pushdownConds, " AND ")
	if q != "" {
		args = append(args, "%"+q+"%")
		conds = append(conds, fmt.Sprintf(
			"(LOWER(ca.code) LIKE LOWER($%d) OR LOWER(COALESCE(ca.slug, ca.code)) LIKE LOWER($%d) OR LOWER(ca.issuer_g_strkey) LIKE LOWER($%d))",
			len(args), len(args), len(args)))
		// q-search pushdown intentionally NOT added — LIKE patterns
		// on three columns combined with the outer SELECT's
		// `ca.code LIKE` rule don't reduce as predictably as the
		// issuer case, and adding the same chosen_assets predicate
		// in WHERE would double-evaluate the same LIKE for no win.
		// If profiling later shows the q-only path is hot, add a
		// q-side chosen_assets variant. For now the SWR cache covers
		// the small filtered-LIST traffic.
	}
	args = append(args, assetsCursorArgs(cursor, order)...)
	if cursor != "" {
		conds = append(conds, assetsCursorPredicate(order, len(args)))
	}
	args = append(args, limit)
	limitPlaceholder := fmt.Sprintf("$%d", len(args))

	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}
	return listAssetsBaseSelectSQL(pushdownPredicate) + where + assetsOrderBy(order) + " LIMIT " + limitPlaceholder, args
}

// assetsCursorArgs returns the positional args appended for the
// active cursor format. Empty cursor → no args.
func assetsCursorArgs(cursor string, order AssetsOrder) []any {
	if cursor == "" {
		return nil
	}
	if order == AssetsOrderVolume24hUSDDesc {
		vol, assetID := parseVolumeCursor(cursor)
		return []any{vol, assetID}
	}
	obsCount, assetID := parseAssetCursor(cursor)
	return []any{obsCount, assetID}
}

// assetsCursorPredicate returns the WHERE clause that resumes
// pagination strictly past the supplied cursor under the active
// ordering. `argEnd` is the index of the last cursor placeholder.
func assetsCursorPredicate(order AssetsOrder, argEnd int) string {
	if order == AssetsOrderVolume24hUSDDesc {
		// Mixed-direction tuple compare: volume DESC, asset_id ASC.
		// Encode as `(v < cv) OR (v = cv AND asset_id > cid)`.
		// COALESCE-to-zero so NULL volumes compare as 0 (sorts last).
		return fmt.Sprintf(
			"(COALESCE(vol.vol_usd, 0) < $%d::numeric "+
				"OR (COALESCE(vol.vol_usd, 0) = $%d::numeric AND ca.asset_id > $%d))",
			argEnd-1, argEnd-1, argEnd)
	}
	return fmt.Sprintf(
		"(ca.observation_count, ca.asset_id) < ($%d, $%d)",
		argEnd-1, argEnd)
}

func assetsOrderBy(order AssetsOrder) string {
	if order == AssetsOrderVolume24hUSDDesc {
		return " ORDER BY COALESCE(vol.vol_usd, 0) DESC, ca.asset_id ASC"
	}
	return " ORDER BY ca.observation_count DESC, ca.asset_id ASC"
}

// parseVolumeCursor decodes a `<vol_or_blank>:<asset_id>` cursor.
// Empty volume sorts as 0 (joins the null-volume tail).
func parseVolumeCursor(cursor string) (vol, assetID string) {
	for i := 0; i < len(cursor); i++ {
		if cursor[i] == ':' {
			v := cursor[:i]
			if v == "" {
				v = "0"
			}
			return v, cursor[i+1:]
		}
	}
	return "0", ""
}

// AssetPricePoint is one hourly USD-price sample in a price-history
// series. `T` is the bucket end (RFC 3339); `P` is the USD price
// rounded to 10 dp via the same direct-or-XLM-triangulated path
// as price_usd. Pointer P so an hour with no trades comes back
// as null rather than zero.
type AssetPricePoint struct {
	T string
	P *string
}

// GetAssetPriceHistory24h returns up to 24 hourly USD price samples
// for the asset, ordered by bucket ASC (oldest first). Each
// sample uses the same direct-then-XLM-triangulated path as
// price_usd, but bucketed to the 1-hour grain. Powers a sparkline
// column on /assets and a price chart preview on the detail page.
//
// Buckets with no underlying trades produce a null P. Callers can
// either render a gap or interpolate; we leave that to the UI.
func (s *Store) GetAssetPriceHistory24h(ctx context.Context, assetID string) ([]AssetPricePoint, error) {
	const q = `
		WITH hours AS (
		  SELECT generate_series(
		    date_trunc('hour', now() - INTERVAL '23 hours'),
		    date_trunc('hour', now()),
		    INTERVAL '1 hour'
		  ) AS bucket
		),
		direct_per_hour AS (
		  SELECT date_trunc('hour', bucket) AS h, last(vwap, bucket)::numeric AS vwap
		    FROM prices_1m
		   WHERE base_asset = $1
		     AND quote_asset = 'fiat:USD'
		     AND bucket >= date_trunc('hour', now() - INTERVAL '23 hours')
		     AND vwap IS NOT NULL
		   GROUP BY h
		),
		asset_xlm_per_hour AS (
		  SELECT date_trunc('hour', bucket) AS h, last(vwap, bucket)::numeric AS vwap
		    FROM prices_1m
		   WHERE base_asset = $1
		     AND quote_asset = 'native'
		     AND bucket >= date_trunc('hour', now() - INTERVAL '23 hours')
		     AND vwap IS NOT NULL
		   GROUP BY h
		),
		xlm_usd_per_hour AS (
		  -- Same stablecoin-proxy fallback as the listing query —
		  -- prices_1m doesn't carry (native, fiat:USD) rows.
		  SELECT date_trunc('hour', bucket) AS h, last(vwap, bucket)::numeric AS vwap
		    FROM prices_1m
		   WHERE base_asset = 'native'
		     AND quote_asset IN (
		       'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
		       'fiat:USD'
		     )
		     AND bucket >= date_trunc('hour', now() - INTERVAL '23 hours')
		     AND vwap IS NOT NULL
		   GROUP BY h
		)
		SELECT
		    to_char(hours.bucket, 'YYYY-MM-DD"T"HH24:MI:SS"Z"') AS t,
		    ROUND(COALESCE(
		      -- XLM itself: use the xlm_usd_per_hour CTE directly.
		      -- Without this, XLM's sparkline is all nulls because
		      -- direct_per_hour and asset_xlm_per_hour both filter
		      -- on base_asset=$1 (= 'native') with quote_asset
		      -- 'fiat:USD' / 'native' — neither has rows.
		      CASE WHEN $1 = 'native' THEN xu.vwap ELSE NULL END,
		      d.vwap,
		      x.vwap * xu.vwap
		    ), 10)::text AS p
		  FROM hours
		  LEFT JOIN direct_per_hour     d  ON d.h  = hours.bucket
		  LEFT JOIN asset_xlm_per_hour  x  ON x.h  = hours.bucket
		  LEFT JOIN xlm_usd_per_hour    xu ON xu.h = hours.bucket
		 ORDER BY hours.bucket ASC
	`
	rows, err := s.db.QueryContext(ctx, q, assetID)
	if err != nil {
		return nil, fmt.Errorf("timescale: GetAssetPriceHistory24h: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]AssetPricePoint, 0, 24)
	for rows.Next() {
		var pt AssetPricePoint
		var p sql.NullString
		if err := rows.Scan(&pt.T, &p); err != nil {
			return nil, fmt.Errorf("timescale: GetAssetPriceHistory24h scan: %w", err)
		}
		if p.Valid && p.String != "" {
			s := p.String
			pt.P = &s
		}
		out = append(out, pt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: GetAssetPriceHistory24h rows: %w", err)
	}
	return out, nil
}

// GetAssetPriceHistory7d returns up to 7 daily USD price samples
// for the asset, ordered by bucket ASC (oldest first). Same
// direct-then-XLM-triangulated path as GetAssetPriceHistory24h, but
// bucketed to the 1-day grain over the last 7 days.
//
// Buckets with no underlying trades produce a null P.
func (s *Store) GetAssetPriceHistory7d(ctx context.Context, assetID string) ([]AssetPricePoint, error) {
	const q = `
		WITH days AS (
		  SELECT generate_series(
		    date_trunc('day', now() - INTERVAL '6 days'),
		    date_trunc('day', now()),
		    INTERVAL '1 day'
		  ) AS bucket
		),
		direct_per_day AS (
		  SELECT date_trunc('day', bucket) AS d, last(vwap, bucket)::numeric AS vwap
		    FROM prices_1m
		   WHERE base_asset = $1
		     AND quote_asset = 'fiat:USD'
		     AND bucket >= date_trunc('day', now() - INTERVAL '6 days')
		     AND vwap IS NOT NULL
		   GROUP BY d
		),
		asset_xlm_per_day AS (
		  SELECT date_trunc('day', bucket) AS d, last(vwap, bucket)::numeric AS vwap
		    FROM prices_1m
		   WHERE base_asset = $1
		     AND quote_asset = 'native'
		     AND bucket >= date_trunc('day', now() - INTERVAL '6 days')
		     AND vwap IS NOT NULL
		   GROUP BY d
		),
		xlm_usd_per_day AS (
		  SELECT date_trunc('day', bucket) AS d, last(vwap, bucket)::numeric AS vwap
		    FROM prices_1m
		   WHERE base_asset = 'native'
		     AND quote_asset IN (
		       'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
		       'fiat:USD'
		     )
		     AND bucket >= date_trunc('day', now() - INTERVAL '6 days')
		     AND vwap IS NOT NULL
		   GROUP BY d
		)
		SELECT
		    to_char(days.bucket, 'YYYY-MM-DD"T"HH24:MI:SS"Z"') AS t,
		    ROUND(COALESCE(
		      CASE WHEN $1 = 'native' THEN xu.vwap ELSE NULL END,
		      d.vwap,
		      x.vwap * xu.vwap
		    ), 10)::text AS p
		  FROM days
		  LEFT JOIN direct_per_day      d  ON d.d  = days.bucket
		  LEFT JOIN asset_xlm_per_day   x  ON x.d  = days.bucket
		  LEFT JOIN xlm_usd_per_day     xu ON xu.d = days.bucket
		 ORDER BY days.bucket ASC
	`
	rows, err := s.db.QueryContext(ctx, q, assetID)
	if err != nil {
		return nil, fmt.Errorf("timescale: GetAssetPriceHistory7d: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]AssetPricePoint, 0, 7)
	for rows.Next() {
		var pt AssetPricePoint
		var p sql.NullString
		if err := rows.Scan(&pt.T, &p); err != nil {
			return nil, fmt.Errorf("timescale: GetAssetPriceHistory7d scan: %w", err)
		}
		if p.Valid && p.String != "" {
			s := p.String
			pt.P = &s
		}
		out = append(out, pt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: GetAssetPriceHistory7d rows: %w", err)
	}
	return out, nil
}

// AssetATH is the asset's all-time-high USD price plus the day
// it was observed. Computed across every USD-quoted day-bucket
// in `prices_1d` (direct USD-stablecoin pairs and `fiat:USD`).
// Triangulated paths (asset/XLM × XLM/USD) are intentionally
// excluded — they introduce two layers of price-discovery
// noise and a single bad XLM/USD reading on a thin day could
// fabricate an ATH.
//
// The metric is "highest day-VWAP" rather than "highest single
// tick" — the day-bucket VWAP is volume-weighted and naturally
// rejects sub-stroop dust prints. The earlier `max(quote/base)`
// definition (R-008 in `docs/review-2026-05-10.md`) put XLM's
// ATH at $1.03 because a single 1-stroop ↔ 1-stroop SDEX dust
// trade pegged the day's max. CoinGecko / CMC use single-tick
// highs across hour buckets that are themselves smoothed; we
// don't have that smoothing layer pre-launch, so day-VWAP is
// the closest dust-resistant approximation.
//
// USD-quote allowlist note (R-008 follow-up, 2026-06-12): the
// `USDT-GCQTGZQQ…` issuer was REMOVED from every USD allowlist —
// there is no Tether on Stellar (the verified catalogue lists no
// stellar network for USDT); that asset trades unpegged (~\-e.09),
// which fabricated an XLM "ATH" of \.78 on thin Jan-2025 days
// (volume_usd=0 dust). USD proxies are the verified USDC issuer +
// fiat:USD only; new proxies require a verified-catalogue entry.
type AssetATH struct {
	USD string // numeric, fixed-point string (preserves precision)
	At  string // RFC-3339 day-bucket the high was set
}

// GetAssetATH returns the asset's all-time-high USD price.
//
// Sources `prices_1d` filtered to USD-denominated quotes — i.e.
// the canonical USDC issuer, plus the synthetic `fiat:USD`
// quote used by off-chain CEX feeds. Returns the (vwap, bucket_day)
// tuple where vwap is maximal.
//
// For native XLM the asset is on the BASE side of every USD pair,
// so the same query works without a special case. Returns
// (nil, nil) cleanly when the asset has never had a USD-quoted
// day with non-null vwap (very thin assets).
func (s *Store) GetAssetATH(ctx context.Context, assetID string) (*AssetATH, error) {
	const q = `
		SELECT
		    vwap::text,
		    to_char(bucket, 'YYYY-MM-DD"T"00:00:00"Z"')
		  FROM prices_1d
		 WHERE base_asset = $1
		   AND quote_asset IN (
		     'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
		     'fiat:USD'
		   )
		   AND vwap IS NOT NULL
		 ORDER BY vwap DESC
		 LIMIT 1
	`
	var ath AssetATH
	switch err := s.db.QueryRowContext(ctx, q, assetID).Scan(&ath.USD, &ath.At); {
	case err == sql.ErrNoRows:
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("timescale: GetAssetATH: %w", err)
	}
	return &ath, nil
}

// GetAssetsATHBatch returns ATH USD price + day for each asset_id
// in a single round trip. DISTINCT ON picks the (vwap-max,
// bucket) tuple per base_asset; the same USD-quote allowlist as
// the per-asset GetAssetATH and the same dust-resistance rationale
// (see AssetATH docs).
//
// Empty input returns an empty map cleanly. Asset_ids with no
// USD-quoted history are simply absent from the result map.
//
// Powers `?include=ath` on /v1/coins so /assets can show "% from
// ATH" without N+1 round trips.
func (s *Store) GetAssetsATHBatch(ctx context.Context, assetIDs []string) (map[string]AssetATH, error) {
	out := make(map[string]AssetATH, len(assetIDs))
	if len(assetIDs) == 0 {
		return out, nil
	}
	const q = `
		SELECT DISTINCT ON (base_asset)
		    base_asset,
		    vwap::text,
		    to_char(bucket, 'YYYY-MM-DD"T"00:00:00"Z"')
		  FROM prices_1d
		 WHERE base_asset = ANY($1)
		   AND quote_asset IN (
		     'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
		     'fiat:USD'
		   )
		   AND vwap IS NOT NULL
		 ORDER BY base_asset, vwap DESC
	`
	rows, err := s.db.QueryContext(ctx, q, pq.Array(assetIDs))
	if err != nil {
		return nil, fmt.Errorf("timescale: GetAssetsATHBatch: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var assetID string
		var ath AssetATH
		if err := rows.Scan(&assetID, &ath.USD, &ath.At); err != nil {
			return nil, fmt.Errorf("timescale: GetAssetsATHBatch scan: %w", err)
		}
		out[assetID] = ath
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: GetAssetsATHBatch rows: %w", err)
	}
	return out, nil
}

// AssetTopMarket is one entry in the top-markets preview returned
// alongside a single asset lookup. Compact summary suitable for an
// asset detail page header — the full markets list lives on
// /v1/markets.
type AssetTopMarket struct {
	Counterparty  string  // the OTHER side of the pair (the side that's NOT this asset)
	Side          string  // "base" if this asset was base, else "quote"
	Volume24hUSD  *string // trailing-24h USD volume for this pair; nil if no USD-equivalent trades
	TradeCount24h int64
}

// GetAssetMarketsCount returns the count of distinct (base, quote)
// pairs the asset participated in over the trailing 24h with a
// non-null prices_1m bucket. Cheaper than GetAssetTopMarkets — no
// volume aggregation, no limit, no ordering. Powers the asset
// detail page's "Markets: 12" header chip.
//
// Returns 0 cleanly when the asset has no rows in the window.
func (s *Store) GetAssetMarketsCount(ctx context.Context, assetID string) (int64, error) {
	const q = `
		SELECT COUNT(*) FROM (
		  SELECT DISTINCT base_asset, quote_asset
		    FROM prices_1m
		   WHERE bucket >= now() - INTERVAL '24 hours'
		     AND (base_asset = $1 OR quote_asset = $1)
		) t
	`
	var n int64
	if err := s.db.QueryRowContext(ctx, q, assetID).Scan(&n); err != nil {
		return 0, fmt.Errorf("timescale: GetAssetMarketsCount: %w", err)
	}
	return n, nil
}

// GetAssetTradeCount24h returns the count of trades the given asset
// participated in (as base OR quote) over the trailing 24 hours.
// Reads the `trades` hypertable directly — accurate down to the
// individual trade rather than the prices_1m bucket aggregation
// used by GetAssetMarketsCount.
//
// Powers the asset detail page header — `observation_count` shows
// the all-time figure, this gives the "what's happening right now"
// counterpart so a user can tell at a glance whether the asset is
// active or a long tail entry.
//
// Returns 0 cleanly when the asset has no trades in the window.
func (s *Store) GetAssetTradeCount24h(ctx context.Context, assetID string) (int64, error) {
	const q = `
		SELECT COUNT(*)
		  FROM trades
		 WHERE ts >= now() - INTERVAL '24 hours'
		   AND (base_asset = $1 OR quote_asset = $1)
	`
	var n int64
	if err := s.db.QueryRowContext(ctx, q, assetID).Scan(&n); err != nil {
		return 0, fmt.Errorf("timescale: GetAssetTradeCount24h: %w", err)
	}
	return n, nil
}

// GetAssetTopMarkets returns up to `limit` markets the given asset
// participates in (as base OR quote), ordered by trailing-24h USD
// volume desc. Used by the explorer asset-detail page to show a
// "Top markets" preview without a separate /v1/markets call.
//
// limit clamps to [1, 20]; default 5.
func (s *Store) GetAssetTopMarkets(ctx context.Context, assetID string, limit int) ([]AssetTopMarket, error) {
	if limit <= 0 || limit > 20 {
		limit = 5
	}
	const q = `
		WITH per_pair_24h AS (
		  SELECT base_asset, quote_asset,
		         SUM(volume_usd)::text AS vol_usd
		    FROM prices_1m
		   WHERE bucket >= now() - INTERVAL '24 hours'
		     AND volume_usd IS NOT NULL
		     AND (base_asset = $1 OR quote_asset = $1)
		   GROUP BY base_asset, quote_asset
		),
		per_pair_count AS (
		  SELECT base_asset, quote_asset,
		         COUNT(*) FILTER (WHERE ts > now() - INTERVAL '24 hours') AS n
		    FROM trades
		   WHERE ts >= now() - INTERVAL '24 hours'
		     AND (base_asset = $1 OR quote_asset = $1)
		   GROUP BY base_asset, quote_asset
		)
		SELECT
		    CASE WHEN p.base_asset = $1 THEN p.quote_asset ELSE p.base_asset END AS counterparty,
		    CASE WHEN p.base_asset = $1 THEN 'base' ELSE 'quote' END             AS side,
		    p.vol_usd                                                            AS vol_24h_usd,
		    COALESCE(c.n, 0)                                                     AS n_24h
		  FROM per_pair_24h p
		  LEFT JOIN per_pair_count c
		    ON c.base_asset = p.base_asset AND c.quote_asset = p.quote_asset
		 ORDER BY p.vol_usd::numeric DESC NULLS LAST
		 LIMIT $2
	`
	rows, err := s.db.QueryContext(ctx, q, assetID, limit)
	if err != nil {
		return nil, fmt.Errorf("timescale: GetAssetTopMarkets: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]AssetTopMarket, 0, limit)
	for rows.Next() {
		var m AssetTopMarket
		var vol sql.NullString
		if err := rows.Scan(&m.Counterparty, &m.Side, &vol, &m.TradeCount24h); err != nil {
			return nil, fmt.Errorf("timescale: GetAssetTopMarkets scan: %w", err)
		}
		if vol.Valid && vol.String != "" && vol.String != "0" {
			v := vol.String
			m.Volume24hUSD = &v
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: GetAssetTopMarkets rows: %w", err)
	}
	return out, nil
}

// GetAssetBySlug returns one row matching the given slug. Returns
// sql.ErrNoRows when the slug doesn't match a known classic asset.
//
// Mirrors ListAssets's per-row metric shape (price/volume/market cap/
// supply) so the explorer can render an asset detail page from a
// single endpoint without scanning the top-N listing first.
// getAssetBySlugSQL is the SQL behind GetAssetBySlug. Hoisted out
// of the function body to keep GetAssetBySlug under the funlen
// threshold; the helpers above already document the chosen-CTE
// pattern that keeps the volume sum and price triangulation on
// the same canonical row.
const getAssetBySlugSQL = `
		WITH chosen AS (
		  -- Three input shapes accepted (in tiebreak preference):
		  --   1. friendly slug (USDC, AQUA, EURC)        — slug column
		  --   2. raw code without issuer (USDC, AQUA)    — code column
		  --   3. canonical asset_id (USDC-GA5Z…)         — asset_id column
		  -- The OR-WHERE catches all three; the ORDER BY tiebreaks in
		  -- preference order so a slug input wins over a code-only
		  -- collision (the disambiguation guard from #45 / scam-token
		  -- protection still applies on the friendly-slug path because
		  -- a curated slug column value beats every code-only match).
		  -- Pre-2026-05-10 canonical asset_id form (CODE-ISSUER) 404'd
		  -- because the WHERE only matched COALESCE(slug, code).
		  SELECT asset_id
		    FROM classic_assets
		   WHERE COALESCE(slug, code) = $1
		      OR asset_id = $1
		   ORDER BY (slug = $1) DESC NULLS LAST,
		            (asset_id = $1) DESC NULLS LAST,
		            observation_count DESC, asset_id ASC
		   LIMIT 1
		),
		per_asset_24h_vol AS (
		  SELECT SUM(volume_usd) AS vol_usd
		    FROM (
		      SELECT volume_usd FROM prices_1m
		       WHERE base_asset = (SELECT asset_id FROM chosen)
		         AND bucket >= now() - INTERVAL '24 hours'
		         AND bucket  <  now()
		         AND volume_usd IS NOT NULL
		      UNION ALL
		      SELECT volume_usd FROM prices_1m
		       WHERE quote_asset = (SELECT asset_id FROM chosen)
		         AND bucket >= now() - INTERVAL '24 hours'
		         AND bucket  <  now()
		         AND volume_usd IS NOT NULL
		    ) t
		),
		direct_usd AS (
		  SELECT vwap FROM prices_1m
		   WHERE base_asset  = (SELECT asset_id FROM chosen)
		     AND quote_asset = 'fiat:USD'
		     AND bucket >= now() - INTERVAL '7 days'
		     AND vwap IS NOT NULL
		   ORDER BY bucket DESC LIMIT 1
		),
		direct_usd_1h AS (
		  SELECT vwap FROM prices_1m
		   WHERE base_asset  = (SELECT asset_id FROM chosen)
		     AND quote_asset = 'fiat:USD'
		     AND bucket BETWEEN now() - INTERVAL '90 minutes'
		                   AND now() - INTERVAL '55 minutes'
		     AND vwap IS NOT NULL
		   ORDER BY bucket DESC LIMIT 1
		),
		direct_usd_24h AS (
		  SELECT vwap FROM prices_1m
		   WHERE base_asset  = (SELECT asset_id FROM chosen)
		     AND quote_asset = 'fiat:USD'
		     AND bucket BETWEEN now() - INTERVAL '26 hours'
		                   AND now() - INTERVAL '23 hours 30 minutes'
		     AND vwap IS NOT NULL
		   ORDER BY bucket DESC LIMIT 1
		),
		direct_usd_7d AS (
		  SELECT vwap FROM prices_1m
		   WHERE base_asset  = (SELECT asset_id FROM chosen)
		     AND quote_asset = 'fiat:USD'
		     AND bucket BETWEEN now() - INTERVAL '7 days 12 hours'
		                   AND now() - INTERVAL '6 days 22 hours'
		     AND vwap IS NOT NULL
		   ORDER BY bucket DESC LIMIT 1
		),
		asset_vs_xlm AS (
		  SELECT vwap FROM prices_1m
		   WHERE base_asset  = (SELECT asset_id FROM chosen)
		     AND quote_asset = 'native'
		     AND bucket >= now() - INTERVAL '7 days'
		     AND vwap IS NOT NULL
		   ORDER BY bucket DESC LIMIT 1
		),
		asset_vs_xlm_1h AS (
		  SELECT vwap FROM prices_1m
		   WHERE base_asset  = (SELECT asset_id FROM chosen)
		     AND quote_asset = 'native'
		     AND bucket BETWEEN now() - INTERVAL '90 minutes'
		                   AND now() - INTERVAL '55 minutes'
		     AND vwap IS NOT NULL
		   ORDER BY bucket DESC LIMIT 1
		),
		asset_vs_xlm_24h AS (
		  SELECT vwap FROM prices_1m
		   WHERE base_asset  = (SELECT asset_id FROM chosen)
		     AND quote_asset = 'native'
		     AND bucket BETWEEN now() - INTERVAL '26 hours'
		                   AND now() - INTERVAL '23 hours 30 minutes'
		     AND vwap IS NOT NULL
		   ORDER BY bucket DESC LIMIT 1
		),
		asset_vs_xlm_7d AS (
		  SELECT vwap FROM prices_1m
		   WHERE base_asset  = (SELECT asset_id FROM chosen)
		     AND quote_asset = 'native'
		     AND bucket BETWEEN now() - INTERVAL '7 days 12 hours'
		                   AND now() - INTERVAL '6 days 22 hours'
		     AND vwap IS NOT NULL
		   ORDER BY bucket DESC LIMIT 1
		),
		xlm_usd AS (
		  -- Same stablecoin-proxy policy as the listing query:
		  -- prices_1m doesn't carry (native, fiat:USD) rows; use
		  -- on-chain XLM/USDC as the USD-equivalent.
		  SELECT vwap FROM prices_1m
		   WHERE base_asset = 'native'
		     AND quote_asset IN (
		       'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
		       'fiat:USD'
		     )
		     AND vwap IS NOT NULL
		     AND bucket >= now() - INTERVAL '24 hours'
		   ORDER BY bucket DESC LIMIT 1
		),
		xlm_usd_1h AS (
		  -- 1h-ago XLM/USD for change_1h_pct via stablecoin proxy.
		  SELECT vwap FROM prices_1m
		   WHERE base_asset = 'native'
		     AND quote_asset IN (
		       'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
		       'fiat:USD'
		     )
		     AND bucket BETWEEN now() - INTERVAL '90 minutes'
		                   AND now() - INTERVAL '55 minutes'
		     AND vwap IS NOT NULL
		   ORDER BY bucket DESC LIMIT 1
		),
		xlm_usd_24h AS (
		  -- 24h-ago XLM/USD for change_24h_pct via stablecoin proxy.
		  SELECT vwap FROM prices_1m
		   WHERE base_asset = 'native'
		     AND quote_asset IN (
		       'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
		       'fiat:USD'
		     )
		     AND bucket BETWEEN now() - INTERVAL '26 hours'
		                   AND now() - INTERVAL '23 hours 30 minutes'
		     AND vwap IS NOT NULL
		   ORDER BY bucket DESC LIMIT 1
		),
		xlm_usd_7d AS (
		  -- 7d-ago XLM/USD for change_7d_pct via stablecoin proxy.
		  SELECT vwap FROM prices_1m
		   WHERE base_asset = 'native'
		     AND quote_asset IN (
		       'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
		       'fiat:USD'
		     )
		     AND bucket BETWEEN now() - INTERVAL '7 days 12 hours'
		                   AND now() - INTERVAL '6 days 22 hours'
		     AND vwap IS NOT NULL
		   ORDER BY bucket DESC LIMIT 1
		)
		SELECT
		    COALESCE(ca.slug, ca.code)            AS slug,
		    ca.asset_id, ca.code, ca.issuer_g_strkey,
		    ca.first_seen_ledger, ca.last_seen_ledger, ca.observation_count,
		    -- XLM (asset_id='native') has no rows in direct_usd or
		    -- asset_vs_xlm — its base_asset is 'native' but neither
		    -- (native, fiat:USD) nor (native, native) exists in
		    -- prices_1m. Use the xlm_usd CTE directly.
		    ROUND(COALESCE(
		      CASE WHEN ca.asset_id = 'native'
		           THEN (SELECT vwap FROM xlm_usd)
		           ELSE NULL
		      END,
		      (SELECT vwap FROM direct_usd),
		      (SELECT vwap FROM asset_vs_xlm) * (SELECT vwap FROM xlm_usd)
		    ), 10)::text                          AS price_usd,
		    vol.vol_usd                           AS volume_24h_usd,
		    NULL::numeric                         AS market_cap_usd,
		    NULL::numeric                         AS circulating_supply,
		    CASE
		      WHEN ca.asset_id = 'native'
		           AND (SELECT vwap FROM xlm_usd) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_1h) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_1h) > 0
		      THEN to_char(((SELECT vwap FROM xlm_usd)
		                  / (SELECT vwap FROM xlm_usd_1h) - 1) * 100,
		                  'FM999999990.00')
		      WHEN (SELECT vwap FROM direct_usd) IS NOT NULL
		           AND (SELECT vwap FROM direct_usd_1h) IS NOT NULL
		           AND (SELECT vwap FROM direct_usd_1h) > 0
		      THEN to_char(((SELECT vwap FROM direct_usd)
		                  / (SELECT vwap FROM direct_usd_1h) - 1) * 100,
		                  'FM999999990.00')
		      WHEN (SELECT vwap FROM asset_vs_xlm) IS NOT NULL
		           AND (SELECT vwap FROM asset_vs_xlm_1h) IS NOT NULL
		           AND (SELECT vwap FROM asset_vs_xlm_1h) > 0
		           AND (SELECT vwap FROM xlm_usd) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_1h) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_1h) > 0
		      THEN to_char((((SELECT vwap FROM asset_vs_xlm)    * (SELECT vwap FROM xlm_usd))
		                  / ((SELECT vwap FROM asset_vs_xlm_1h) * (SELECT vwap FROM xlm_usd_1h))
		                  - 1) * 100, 'FM999999990.00')
		      ELSE NULL
		    END                                   AS change_1h_pct,
		    CASE
		      WHEN ca.asset_id = 'native'
		           AND (SELECT vwap FROM xlm_usd) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_24h) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_24h) > 0
		      THEN to_char(((SELECT vwap FROM xlm_usd)
		                  / (SELECT vwap FROM xlm_usd_24h) - 1) * 100,
		                  'FM999999990.00')
		      WHEN (SELECT vwap FROM direct_usd) IS NOT NULL
		           AND (SELECT vwap FROM direct_usd_24h) IS NOT NULL
		           AND (SELECT vwap FROM direct_usd_24h) > 0
		      THEN to_char(((SELECT vwap FROM direct_usd)
		                  / (SELECT vwap FROM direct_usd_24h) - 1) * 100,
		                  'FM999999990.00')
		      WHEN (SELECT vwap FROM asset_vs_xlm) IS NOT NULL
		           AND (SELECT vwap FROM asset_vs_xlm_24h) IS NOT NULL
		           AND (SELECT vwap FROM asset_vs_xlm_24h) > 0
		           AND (SELECT vwap FROM xlm_usd) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_24h) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_24h) > 0
		      THEN to_char((((SELECT vwap FROM asset_vs_xlm)     * (SELECT vwap FROM xlm_usd))
		                  / ((SELECT vwap FROM asset_vs_xlm_24h) * (SELECT vwap FROM xlm_usd_24h))
		                  - 1) * 100, 'FM999999990.00')
		      ELSE NULL
		    END                                   AS change_24h_pct,
		    CASE
		      WHEN ca.asset_id = 'native'
		           AND (SELECT vwap FROM xlm_usd) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_7d) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_7d) > 0
		      THEN to_char(((SELECT vwap FROM xlm_usd)
		                  / (SELECT vwap FROM xlm_usd_7d) - 1) * 100,
		                  'FM999999990.00')
		      WHEN (SELECT vwap FROM direct_usd) IS NOT NULL
		           AND (SELECT vwap FROM direct_usd_7d) IS NOT NULL
		           AND (SELECT vwap FROM direct_usd_7d) > 0
		      THEN to_char(((SELECT vwap FROM direct_usd)
		                  / (SELECT vwap FROM direct_usd_7d) - 1) * 100,
		                  'FM999999990.00')
		      WHEN (SELECT vwap FROM asset_vs_xlm) IS NOT NULL
		           AND (SELECT vwap FROM asset_vs_xlm_7d) IS NOT NULL
		           AND (SELECT vwap FROM asset_vs_xlm_7d) > 0
		           AND (SELECT vwap FROM xlm_usd) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_7d) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_7d) > 0
		      THEN to_char((((SELECT vwap FROM asset_vs_xlm)    * (SELECT vwap FROM xlm_usd))
		                  / ((SELECT vwap FROM asset_vs_xlm_7d) * (SELECT vwap FROM xlm_usd_7d))
		                  - 1) * 100, 'FM999999990.00')
		      ELSE NULL
		    END                                   AS change_7d_pct
		  FROM chosen
		  JOIN classic_assets ca ON ca.asset_id = chosen.asset_id
		  LEFT JOIN per_asset_24h_vol vol ON true
`

// GetAssetBySlug looks up by friendly slug (USDC, AQUA, EURC),
// canonical asset_id (USDC-GA5Z…), OR raw code with case-insensitive
// retry — see the SQL's WHERE clause + the handler's case-fallback
// for the full input-shape table.
func (s *Store) GetAssetBySlug(ctx context.Context, slug string) (AssetRow, error) {
	r, err := scanAssetRow(s.db.QueryRowContext(ctx, getAssetBySlugSQL, slug))
	if err != nil {
		// Surface sql.ErrNoRows unwrapped so handler errors.Is checks
		// keep matching; scanAssetRow wraps with %w which preserves it.
		return AssetRow{}, err
	}
	return r, nil
}

// GetAssetByAssetID is a thin wrapper kept for clarity at the
// handler layer — the underlying SQL accepts canonical asset_id
// alongside friendly slug since 2026-05-10, so this just calls
// GetAssetBySlug. (Pre-fix the canonical form 404'd; see the
// alerts-catalog row "Asset canonical asset_id 404" for context.)
func (s *Store) GetAssetByAssetID(ctx context.Context, assetID string) (AssetRow, error) {
	return s.GetAssetBySlug(ctx, assetID)
}

// GetNativeAssetRow returns the synthetic AssetRow for native XLM.
//
// Native XLM has no row in classic_assets — that table only tracks
// issued classic assets, by definition. Without a special-case path
// the public lookup `/v1/coins/XLM` either 404s (no slug match) or
// returns whichever issued token's code happens to be "XLM" wins
// the disambiguation tiebreak (today: a scam token issued by
// GAE5PQNUIP5E…).
//
// Population:
//   - Slug / Code: hardcoded "XLM"
//   - AssetID: "native" (the canonical pair-side identifier)
//   - IssuerGStrkey: "" (native has no issuer)
//   - First/Last seen ledger: the trades hypertable's min/max for
//     base_asset='native' OR quote_asset='native'
//   - ObservationCount: total trades touching native in the
//     hypertable, capped at int64
//   - PriceUSD + Change*Pct: same xlm_usd / xlm_usd_{1h,24h,7d}
//     stablecoin-proxy chain used by GetAssetBySlug + the listing
//     query for non-native assets
//   - Volume24hUSD: SUM(volume_usd) where the asset is base or quote
//     in the trailing 24h
//   - MarketCapUSD / CirculatingSupply: NULL — supply pipeline
//     doesn't yet emit a row for native (algorithm 1 work)
//
// Always returns a populated row (no sql.ErrNoRows path) — the
// underlying CTEs LEFT JOIN out to NULL when there's no data.
func (s *Store) GetNativeAssetRow(ctx context.Context) (AssetRow, error) {
	return scanAssetRow(s.db.QueryRowContext(ctx, getNativeAssetSQL))
}

// getNativeAssetSQL is GetNativeAssetRow's query, hoisted to a
// package constant so the function body stays under the funlen
// threshold. Returns the same column shape as listAssetsBaseSelect
// + getAssetBySlugSQL — the shared scanAssetRow projector handles
// it identically.
const getNativeAssetSQL = `
		WITH per_asset_24h_vol AS (
		  SELECT SUM(volume_usd) AS vol_usd
		    FROM (
		      SELECT volume_usd FROM prices_1m
		       WHERE base_asset = 'native'
		         AND bucket >= now() - INTERVAL '24 hours'
		         AND bucket  <  now()
		         AND volume_usd IS NOT NULL
		      UNION ALL
		      SELECT volume_usd FROM prices_1m
		       WHERE quote_asset = 'native'
		         AND bucket >= now() - INTERVAL '24 hours'
		         AND bucket  <  now()
		         AND volume_usd IS NOT NULL
		    ) t
		),
		xlm_usd AS (
		  SELECT vwap FROM prices_1m
		   WHERE base_asset = 'native'
		     AND quote_asset IN (
		       'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
		       'fiat:USD'
		     )
		     AND vwap IS NOT NULL
		     AND bucket >= now() - INTERVAL '24 hours'
		   ORDER BY bucket DESC LIMIT 1
		),
		xlm_usd_1h AS (
		  SELECT vwap FROM prices_1m
		   WHERE base_asset = 'native'
		     AND quote_asset IN (
		       'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
		       'fiat:USD'
		     )
		     AND bucket BETWEEN now() - INTERVAL '90 minutes'
		                   AND now() - INTERVAL '55 minutes'
		     AND vwap IS NOT NULL
		   ORDER BY bucket DESC LIMIT 1
		),
		xlm_usd_24h AS (
		  SELECT vwap FROM prices_1m
		   WHERE base_asset = 'native'
		     AND quote_asset IN (
		       'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
		       'fiat:USD'
		     )
		     AND bucket BETWEEN now() - INTERVAL '26 hours'
		                   AND now() - INTERVAL '23 hours 30 minutes'
		     AND vwap IS NOT NULL
		   ORDER BY bucket DESC LIMIT 1
		),
		xlm_usd_7d AS (
		  SELECT vwap FROM prices_1m
		   WHERE base_asset = 'native'
		     AND quote_asset IN (
		       'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
		       'fiat:USD'
		     )
		     AND bucket BETWEEN now() - INTERVAL '7 days 12 hours'
		                   AND now() - INTERVAL '6 days 22 hours'
		     AND vwap IS NOT NULL
		   ORDER BY bucket DESC LIMIT 1
		),
		ledger_bounds AS (
		  -- Always return one row with placeholder zeros — the
		  -- previous version scanned the trades hypertable for
		  -- 7 days OF native-touching rows, which is millions of
		  -- rows on a busy ledger and timed out under lock-table
		  -- pressure (53200). Native XLM is well-known; the
		  -- explorer doesn't need an accurate first_seen_ledger
		  -- for it. observation_count uses prices_1m row count
		  -- as a cheap proxy.
		  SELECT
		    0::bigint AS first_ledger,
		    0::bigint AS last_ledger,
		    COALESCE(
		      (SELECT COUNT(*)::bigint
		         FROM prices_1m
		        WHERE bucket >= now() - INTERVAL '24 hours'
		          AND (base_asset = 'native' OR quote_asset = 'native')),
		      0
		    ) AS obs_count
		)
		SELECT
		    'XLM'                                  AS slug,
		    'native'                               AS asset_id,
		    'XLM'                                  AS code,
		    ''                                     AS issuer_g_strkey,
		    lb.first_ledger                        AS first_seen_ledger,
		    lb.last_ledger                         AS last_seen_ledger,
		    lb.obs_count                           AS observation_count,
		    ROUND((SELECT vwap FROM xlm_usd), 10)::text AS price_usd,
		    vol.vol_usd                            AS volume_24h_usd,
		    NULL::numeric                          AS market_cap_usd,
		    NULL::numeric                          AS circulating_supply,
		    CASE
		      WHEN (SELECT vwap FROM xlm_usd) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_1h) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_1h) > 0
		      THEN to_char(((SELECT vwap FROM xlm_usd)
		                  / (SELECT vwap FROM xlm_usd_1h) - 1) * 100,
		                  'FM999999990.00')
		      ELSE NULL
		    END                                    AS change_1h_pct,
		    CASE
		      WHEN (SELECT vwap FROM xlm_usd) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_24h) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_24h) > 0
		      THEN to_char(((SELECT vwap FROM xlm_usd)
		                  / (SELECT vwap FROM xlm_usd_24h) - 1) * 100,
		                  'FM999999990.00')
		      ELSE NULL
		    END                                    AS change_24h_pct,
		    CASE
		      WHEN (SELECT vwap FROM xlm_usd) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_7d) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_7d) > 0
		      THEN to_char(((SELECT vwap FROM xlm_usd)
		                  / (SELECT vwap FROM xlm_usd_7d) - 1) * 100,
		                  'FM999999990.00')
		      ELSE NULL
		    END                                    AS change_7d_pct
		  FROM ledger_bounds lb
		  LEFT JOIN per_asset_24h_vol vol ON true
`

// LatestAssetStats returns per-asset 24h volume + supply stats
// for /v1/assets/{id}. Volume sums prices_1m.volume_usd across
// pairs where the asset is base or quote (mirrors
// Volume24hUSDForAsset). Supply is null for now — the source
// table classic_asset_stats_5m is unwritten.
//
// Always returns nil error for a row that simply has no stats;
// the LEFT JOINs evaluate to NULL.
func (s *Store) LatestAssetStats(ctx context.Context, assetID string) (AssetRow, error) {
	const q = `
		SELECT COALESCE(SUM(volume_usd), 0)::text
		  FROM (
		    SELECT volume_usd FROM prices_1m
		     WHERE base_asset = $1
		       AND bucket >= now() - INTERVAL '24 hours'
		       AND bucket  <  now()
		    UNION ALL
		    SELECT volume_usd FROM prices_1m
		     WHERE quote_asset = $1
		       AND bucket >= now() - INTERVAL '24 hours'
		       AND bucket  <  now()
		  ) t
	`
	var vol string
	if err := s.db.QueryRowContext(ctx, q, assetID).Scan(&vol); err != nil {
		return AssetRow{}, fmt.Errorf("timescale: LatestAssetStats: %w", err)
	}
	out := AssetRow{AssetID: assetID}
	if vol != "" && vol != "0" {
		out.Volume24hUSD = &vol
	}
	return out, nil
}

// ValidateAssetsCursor returns an error if `cursor` is non-empty
// but doesn't match the encoded shape the listing emits for the
// active order. Empty cursor is always valid (resume from the
// first page). Callers should reject invalid cursors at the
// handler boundary with a 400 — falling through silently
// truncates the keyset predicate to "(0, \"\")" / "(0, \"\")"
// which matches no rows under the default order, returning an
// empty page that looks like end-of-pagination.
func ValidateAssetsCursor(cursor string, order AssetsOrder) error {
	if cursor == "" {
		return nil
	}
	idx := strings.IndexByte(cursor, ':')
	if idx < 0 {
		return fmt.Errorf("missing ':' separator")
	}
	prefix, suffix := cursor[:idx], cursor[idx+1:]
	if suffix == "" {
		return fmt.Errorf("missing asset_id suffix")
	}
	if order == AssetsOrderVolume24hUSDDesc {
		// Volume prefix may be empty (last row had a null vol_usd)
		// or a Postgres-style numeric: digits with at most one '.'.
		if prefix != "" && !isNumericPrefix(prefix) {
			return fmt.Errorf("non-numeric volume prefix")
		}
		return nil
	}
	if prefix == "" {
		return fmt.Errorf("missing observation_count prefix")
	}
	for j := 0; j < len(prefix); j++ {
		if prefix[j] < '0' || prefix[j] > '9' {
			return fmt.Errorf("non-numeric observation_count prefix")
		}
	}
	return nil
}

// isNumericPrefix returns true for a non-empty digit string with
// at most one '.' separator. Negative volumes don't exist in our
// data, so we don't accept a leading '-'.
func isNumericPrefix(s string) bool {
	dot := false
	for j := 0; j < len(s); j++ {
		c := s[j]
		switch {
		case c >= '0' && c <= '9':
		case c == '.' && !dot:
			dot = true
		default:
			return false
		}
	}
	return true
}

// parseAssetCursor decodes a `<obs_count>:<asset_id>` cursor.
// Empty cursor → (0, "") which means "no cursor". Malformed
// cursors fall through to the same; the handler is responsible
// for rejecting them via ValidateAssetsCursor before this is
// reached.
func parseAssetCursor(cursor string) (obsCount int64, assetID string) {
	if cursor == "" {
		return 0, ""
	}
	for i := 0; i < len(cursor); i++ {
		if cursor[i] == ':' {
			n := int64(0)
			for j := 0; j < i; j++ {
				c := cursor[j]
				if c < '0' || c > '9' {
					return 0, ""
				}
				n = n*10 + int64(c-'0')
			}
			return n, cursor[i+1:]
		}
	}
	return 0, ""
}

// EncodeAssetCursor pairs with parseAssetCursor — the API handler
// emits the encoded form as the next-page cursor in pagination
// meta. Exported so v1/asset_catalogue.go can call it without duplicating.
func EncodeAssetCursor(obsCount int64, assetID string) string {
	return fmt.Sprintf("%d:%s", obsCount, assetID)
}

// GetAssetsPriceHistory24hBatch returns 24h hourly USD-price series
// for many assets in one query. Result is keyed by asset_id; the
// per-asset slice has up to 24 ordered points (oldest → newest).
// Assets with no trade history in the window get an empty slice
// (callers can render that as "no chart").
//
// Same direct-then-XLM-triangulated path as the single-asset
// GetAssetPriceHistory24h; just a single CTE pass over all
// requested assets at once.
func (s *Store) GetAssetsPriceHistory24hBatch(ctx context.Context, assetIDs []string) (map[string][]AssetPricePoint, error) {
	if len(assetIDs) == 0 {
		return map[string][]AssetPricePoint{}, nil
	}
	const q = `
		WITH hours AS (
		  SELECT generate_series(
		    date_trunc('hour', now() - INTERVAL '23 hours'),
		    date_trunc('hour', now()),
		    INTERVAL '1 hour'
		  ) AS bucket
		),
		direct_per_hour AS (
		  SELECT base_asset AS asset_id,
		         date_trunc('hour', bucket) AS h,
		         last(vwap, bucket)::numeric AS vwap
		    FROM prices_1m
		   WHERE base_asset = ANY($1)
		     AND quote_asset = 'fiat:USD'
		     AND bucket >= date_trunc('hour', now() - INTERVAL '23 hours')
		     AND vwap IS NOT NULL
		   GROUP BY base_asset, h
		),
		asset_xlm_per_hour AS (
		  SELECT base_asset AS asset_id,
		         date_trunc('hour', bucket) AS h,
		         last(vwap, bucket)::numeric AS vwap
		    FROM prices_1m
		   WHERE base_asset = ANY($1)
		     AND quote_asset = 'native'
		     AND bucket >= date_trunc('hour', now() - INTERVAL '23 hours')
		     AND vwap IS NOT NULL
		   GROUP BY base_asset, h
		),
		xlm_usd_per_hour AS (
		  SELECT date_trunc('hour', bucket) AS h, last(vwap, bucket)::numeric AS vwap
		    FROM prices_1m
		   WHERE base_asset = 'native'
		     AND quote_asset IN (
		       'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
		       'fiat:USD'
		     )
		     AND bucket >= date_trunc('hour', now() - INTERVAL '23 hours')
		     AND vwap IS NOT NULL
		   GROUP BY h
		),
		want AS (
		  SELECT unnest($1::text[]) AS asset_id
		)
		SELECT
		    w.asset_id,
		    to_char(hours.bucket, 'YYYY-MM-DD"T"HH24:MI:SS"Z"') AS t,
		    ROUND(COALESCE(
		      CASE WHEN w.asset_id = 'native' THEN xu.vwap ELSE NULL END,
		      d.vwap,
		      x.vwap * xu.vwap
		    ), 10)::text AS p
		  FROM want w
		  CROSS JOIN hours
		  LEFT JOIN direct_per_hour     d  ON d.h  = hours.bucket AND d.asset_id  = w.asset_id
		  LEFT JOIN asset_xlm_per_hour  x  ON x.h  = hours.bucket AND x.asset_id  = w.asset_id
		  LEFT JOIN xlm_usd_per_hour    xu ON xu.h = hours.bucket
		 ORDER BY w.asset_id, hours.bucket ASC
	`
	rows, err := s.db.QueryContext(ctx, q, pq.Array(assetIDs))
	if err != nil {
		return nil, fmt.Errorf("timescale: GetAssetsPriceHistory24hBatch: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string][]AssetPricePoint, len(assetIDs))
	for rows.Next() {
		var assetID string
		var pt AssetPricePoint
		var p sql.NullString
		if err := rows.Scan(&assetID, &pt.T, &p); err != nil {
			return nil, fmt.Errorf("timescale: GetAssetsPriceHistory24hBatch scan: %w", err)
		}
		if p.Valid && p.String != "" {
			s := p.String
			pt.P = &s
		}
		out[assetID] = append(out[assetID], pt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: GetAssetsPriceHistory24hBatch rows: %w", err)
	}
	return out, nil
}

// GetAssetsPriceHistory7dBatch returns 7d daily USD-price series for
// many assets in one query. 7-bucket-deep daily grain, mirroring
// the per-asset GetAssetPriceHistory7d. Same direct-then-XLM-
// triangulated path; one query for many asset_ids.
func (s *Store) GetAssetsPriceHistory7dBatch(ctx context.Context, assetIDs []string) (map[string][]AssetPricePoint, error) {
	if len(assetIDs) == 0 {
		return map[string][]AssetPricePoint{}, nil
	}
	const q = `
		WITH days AS (
		  SELECT generate_series(
		    date_trunc('day', now() - INTERVAL '6 days'),
		    date_trunc('day', now()),
		    INTERVAL '1 day'
		  ) AS bucket
		),
		direct_per_day AS (
		  SELECT base_asset AS asset_id,
		         date_trunc('day', bucket) AS d,
		         last(vwap, bucket)::numeric AS vwap
		    FROM prices_1m
		   WHERE base_asset = ANY($1)
		     AND quote_asset = 'fiat:USD'
		     AND bucket >= date_trunc('day', now() - INTERVAL '6 days')
		     AND vwap IS NOT NULL
		   GROUP BY base_asset, d
		),
		asset_xlm_per_day AS (
		  SELECT base_asset AS asset_id,
		         date_trunc('day', bucket) AS d,
		         last(vwap, bucket)::numeric AS vwap
		    FROM prices_1m
		   WHERE base_asset = ANY($1)
		     AND quote_asset = 'native'
		     AND bucket >= date_trunc('day', now() - INTERVAL '6 days')
		     AND vwap IS NOT NULL
		   GROUP BY base_asset, d
		),
		xlm_usd_per_day AS (
		  SELECT date_trunc('day', bucket) AS d, last(vwap, bucket)::numeric AS vwap
		    FROM prices_1m
		   WHERE base_asset = 'native'
		     AND quote_asset IN (
		       'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
		       'fiat:USD'
		     )
		     AND bucket >= date_trunc('day', now() - INTERVAL '6 days')
		     AND vwap IS NOT NULL
		   GROUP BY d
		),
		want AS (
		  SELECT unnest($1::text[]) AS asset_id
		)
		SELECT
		    w.asset_id,
		    to_char(days.bucket, 'YYYY-MM-DD"T"HH24:MI:SS"Z"') AS t,
		    ROUND(COALESCE(
		      CASE WHEN w.asset_id = 'native' THEN xu.vwap ELSE NULL END,
		      d.vwap,
		      x.vwap * xu.vwap
		    ), 10)::text AS p
		  FROM want w
		  CROSS JOIN days
		  LEFT JOIN direct_per_day     d  ON d.d  = days.bucket AND d.asset_id  = w.asset_id
		  LEFT JOIN asset_xlm_per_day  x  ON x.d  = days.bucket AND x.asset_id  = w.asset_id
		  LEFT JOIN xlm_usd_per_day    xu ON xu.d = days.bucket
		 ORDER BY w.asset_id, days.bucket ASC
	`
	rows, err := s.db.QueryContext(ctx, q, pq.Array(assetIDs))
	if err != nil {
		return nil, fmt.Errorf("timescale: GetAssetsPriceHistory7dBatch: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string][]AssetPricePoint, len(assetIDs))
	for rows.Next() {
		var assetID string
		var pt AssetPricePoint
		var p sql.NullString
		if err := rows.Scan(&assetID, &pt.T, &p); err != nil {
			return nil, fmt.Errorf("timescale: GetAssetsPriceHistory7dBatch scan: %w", err)
		}
		if p.Valid && p.String != "" {
			s := p.String
			pt.P = &s
		}
		out[assetID] = append(out[assetID], pt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: GetAssetsPriceHistory7dBatch rows: %w", err)
	}
	return out, nil
}
