// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"fmt"
)

// asset_price_snapshot — the per-asset headline-price rollup behind the
// GET /v1/assets listing (#331 F1).
//
// The listing used to DERIVE its price column per request: twelve
// `DISTINCT ON … FROM prices_1m` CTEs (four USD-quoted lookbacks, four
// XLM-quoted lookbacks each reading both stored directions, four
// XLM/USD scalar lookups) materialised for EVERY asset, on every
// uncached variant, whatever page was asked for. Measured on r1
// 2026-09-02 (`pg_stat_statements`, since 2026-07-06): the unfiltered
// listing statement ran 8,019 times at mean 2,400 ms / max 10,295 ms,
// touching 380,324 shared buffers per call to return ~116 rows; three
// sibling shapes add 10,500 more calls at 1.5–2.1 s. `EXPLAIN (ANALYZE,
// BUFFERS)` on `?limit=50` at HEAD: 1,830 ms, 348,442 buffer hits,
// 51 MB of `external merge` temp spill — of which the eight
// `DISTINCT ON` CTEs were 1,353 ms (the 7-day `asset_vs_xlm` arm alone
// was 881 ms and owned the whole disk sort).
//
// The derivation itself is CORRECT — the problem is only that it ran on
// the request path. So it moves here verbatim ([assetPriceCTEs] is the
// same CTE text the listing carried, minus the pushdown markers a full
// recompute cannot use) and the aggregator folds it into one small
// keyed-on-PK table on the same cadence the sibling volume rollup
// already runs at. The listing then LEFT JOINs `asset_price_snapshot`.
// Same pattern, same reasons, as migration 0087 (`asset_volume_24h`,
// #43) and 0149 (`asset_volume_character`).
//
// Why a plain worker-maintained table and not the two alternatives:
//
//   - NOT a continuous aggregate. A CAGG is `time_bucket(...) GROUP BY`
//     over one source projection. This substrate is "the LATEST row per
//     asset across two USD-proxy quote forms, two XLM identity forms and
//     BOTH stored directions, over a 7-day lookback, plus three
//     point-in-time lookbacks, triangulated through a scalar XLM/USD" —
//     a `DISTINCT ON` over a UNION, which no single `GROUP BY` expresses.
//     Same wall 0087 and 0149 hit.
//   - NOT a materialised view. `REFRESH MATERIALIZED VIEW` takes ACCESS
//     EXCLUSIVE on the relation and would stall every concurrent read of
//     the flagship customer-facing listing for the duration of the
//     recompute; `CONCURRENTLY` avoids the lock but diffs the whole
//     relation and still needs an external caller. The upsert+prune pair
//     below takes row-level locks only — the same reason
//     [Store.RefreshAssetVolume24h] is written this way.
//
// Staleness contract (deliberate, and bounded rather than open-ended):
//
//	prices_1m CAGG lag      <= ~90 s  (30 s schedule + 30 s end_offset,
//	                                   migration 0002)
//	rollup refresh cadence   = 2 min  (assetvolrollup.DefaultInterval)
//	API listing cache TTL    = 2 min  (v1.NewCachedAssetsReader)
//	-------------------------------------------------------------
//	worst-case served age   ~= 5.5 min   (was ~3.5 min before this
//	                                      change: the rollup adds its
//	                                      own cadence, nothing else)
//
// That is older than GET /v1/price, which serves the last CLOSED bucket
// (ADR-0015; ~85–94 s observed on r1). The listing was ALREADY the
// staler of the two — its 2-minute cache saw to that — and this widens
// the gap by the refresh interval, not by an unbounded amount. It is
// also not silent: [assetPriceSnapshotMaxAge] is a HARD ceiling spliced
// into the listing's join, so a wedged or dead aggregator can never
// serve indefinitely-old prices. Past the ceiling the join misses, the
// asset renders exactly as an asset with no price does today
// (`price_usd` absent, rank tier 1), and the aggregator's own
// per-binary heartbeat is what pages. Fail-closed beats a quietly
// ageing money column.
//
// GET /v1/assets/{id} is deliberately NOT moved onto this table: it is a
// single-asset query whose price CTEs are already narrowed to one asset
// and cost milliseconds, so the detail view stays the freshest surface.
// Consequence to know about: a listing row and its own detail page can
// disagree by up to the ceiling above while a price is moving.

// assetPriceSnapshotMaxAge is how old an `asset_price_snapshot` row may
// be and still be served by the listing. Spliced into the listing's
// LEFT JOIN (see [listAssetsBaseSelect]) so the bound is enforced in
// SQL, not by convention.
//
// 15 minutes = 7 missed passes of the 2-minute refresh cadence. Wide
// enough that an aggregator restart, a slow pass or a Postgres hiccup
// never blanks the flagship page; narrow enough that a price cannot
// drift materially before the row stops being served. Changing it means
// changing the staleness contract documented above, which is why it is
// one greppable constant and not an inline literal.
const assetPriceSnapshotMaxAge = "15 minutes"

// snapshotPriceUSDExpr is the USD-price derivation the /v1/assets
// listing used to inline: direct USD-proxy quote first, else the XLM
// price triangulated through XLM/USD, with native XLM taking the
// 24h-bounded xlm_usd scalar ahead of either.
//
// This is the byte-identical twin of the expression that lived in
// asset_catalogue.go as `listingPriceUSDExpr` before #331 F1; the
// listing's constant of that name now reads `aps.price_usd` — this
// column — so there is exactly ONE derivation and the served value is
// the same decimal it always was, only computed on the rollup's cadence
// instead of per request. `ca` is the spine alias in both places.
const snapshotPriceUSDExpr = `COALESCE(
		      CASE WHEN ca.asset_id = 'native'
		           THEN (SELECT vwap FROM xlm_usd)
		           ELSE NULL
		      END,
		      direct.vwap,
		      vs_xlm.vwap * (SELECT vwap FROM xlm_usd)
		    )`

// assetPriceCTEs is the price substrate: four USD-quoted lookbacks
// (`direct_usd*`), four XLM-quoted lookbacks read in BOTH stored
// directions (`asset_vs_xlm*`), and four XLM/USD scalar lookups
// (`xlm_usd*`). Moved here VERBATIM from listAssetsBaseSelect — the
// prose below is the original prose, and TestProxyQuoteLists_Lockstep
// pins its USD/XLM proxy IN-lists to the same one set the coverage
// tripwire and the transitive resolver use.
//
// The `/*PUSHDOWN_BASE*/` / `/*PUSHDOWN_QUOTE*/` markers the listing
// carried are gone: they existed to narrow these CTEs to one issuer's
// assets on a FILTERED listing, and a full all-asset recompute has
// nothing to narrow to (same reason refreshAssetVolumeUpsert carries
// none).
const assetPriceCTEs = `
		direct_usd AS (
		  -- "Direct USD" = quoted in fiat:USD (CEX feeds) OR in
		  -- USDC — the same stablecoin-proxy policy the xlm_usd
		  -- CTEs below already apply (CLAUDE.md: "stablecoin
		  -- fiat-proxy is aggregator policy" — USDC ≈ USD, ~0.1%
		  -- peg error accepted). A USDC-quoted vwap is taken AS
		  -- the USD price. Without the USDC member, assets whose
		  -- only markets quote in USDC (AUDD, EURC, every *allow
		  -- variant…) had 24h volume but a NULL price_usd — 473 of
		  -- 500 listing rows priceless (2026-08-24 operator
		  -- report). USDC counts in BOTH identity forms: the
		  -- classic G-issuer line AND its SAC contract (CCW67T… —
		  -- Soroban-venue trades carry the SAC id; the
		  -- AliasRegistry in internal/canonical/alias.go unifies
		  -- these on serving paths, but this SQL joins literal
		  -- strings, and USDC↔SAC is one of the two compile-time-
		  -- known wrapper families, operator-pinned via r1's
		  -- [supply.sac_wrappers]). DISTINCT ON + bucket DESC
		  -- keeps the freshest row across all quote forms.
		  SELECT DISTINCT ON (base_asset) base_asset AS asset_id, vwap,
		         array_length(sources, 1) AS source_count
		    FROM prices_1m
		   WHERE quote_asset IN (
		     'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
		     'CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75',
		     'fiat:USD'
		   )
		     AND bucket >= now() - INTERVAL '7 days'
		     AND vwap IS NOT NULL
		   ORDER BY base_asset, bucket DESC
		),
		direct_usd_1h AS (
		  SELECT DISTINCT ON (base_asset) base_asset AS asset_id, vwap
		    FROM prices_1m
		   WHERE quote_asset IN (
		     'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
		     'CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75',
		     'fiat:USD'
		   )
		     AND bucket BETWEEN now() - INTERVAL '90 minutes'
		                   AND now() - INTERVAL '55 minutes'
		     AND vwap IS NOT NULL
		   ORDER BY base_asset, bucket DESC
		),
		direct_usd_24h AS (
		  SELECT DISTINCT ON (base_asset) base_asset AS asset_id, vwap
		    FROM prices_1m
		   WHERE quote_asset IN (
		     'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
		     'CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75',
		     'fiat:USD'
		   )
		     AND bucket BETWEEN now() - INTERVAL '26 hours'
		                   AND now() - INTERVAL '23 hours 30 minutes'
		     AND vwap IS NOT NULL
		   ORDER BY base_asset, bucket DESC
		),
		direct_usd_7d AS (
		  SELECT DISTINCT ON (base_asset) base_asset AS asset_id, vwap
		    FROM prices_1m
		   WHERE quote_asset IN (
		     'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
		     'CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75',
		     'fiat:USD'
		   )
		     AND bucket BETWEEN now() - INTERVAL '7 days 12 hours'
		                   AND now() - INTERVAL '6 days 22 hours'
		     AND vwap IS NOT NULL
		   ORDER BY base_asset, bucket DESC
		),
		asset_vs_xlm AS (
		  -- XLM leg in BOTH identity forms: 'native' AND its SAC
		  -- contract (CAS3J7… = canonical.XLMSacContractID,
		  -- alias.go) — soroswap/phoenix/aquarius trades quote in
		  -- the SAC form, which the AliasRegistry unifies on
		  -- serving paths but literal-string SQL must list
		  -- explicitly. Both legs multiply by the same xlm_usd.
		  -- BASE-side identity folding (a SAC-form base_asset row
		  -- surfacing alongside its classic twin — "USDC shows
		  -- twice") is now done in the API listing handler by
		  -- v1.Server.foldAliasTwins, which collapses each
		  -- non-canonical alias row onto its canonical row via the
		  -- same AliasRegistry (task #28 Part A).
		  --
		  -- ...and in BOTH stored DIRECTIONS. Sources that write swap
		  -- direction (aquarius: base = token_in, no canonical.Orient)
		  -- store a token bought with XLM as (XLM-SAC, token) — XLM as
		  -- BASE. Reading only quote_asset IN (XLM forms) left that
		  -- whole market invisible: r1 2026-08-28, CBIJ… had $730k/7d
		  -- against the XLM SAC and served price_usd null with no
		  -- withheld verdict. The inverted arm reads (XLM, token) and
		  -- inverts (vwap is "XLM priced in token", so 1/vwap is the
		  -- token in XLM). The volume path already read both
		  -- directions (soroban_volume.go) — which is exactly why the
		  -- asset had volume and no price.
		  --
		  -- Base-side rows are preferred over inverted ones (ORDER BY
		  -- inverted BEFORE bucket) so every asset that already priced
		  -- keeps byte-identical output; the inverted arm only fills
		  -- assets that had NO base-side row in the window.
		  SELECT DISTINCT ON (asset_id) asset_id, vwap, source_count
		    FROM (
		      SELECT base_asset AS asset_id, vwap,
		             array_length(sources, 1) AS source_count,
		             bucket, 0 AS inverted
		        FROM prices_1m
		       WHERE quote_asset IN ('native', 'CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA')
		         AND bucket >= now() - INTERVAL '7 days'
		         AND vwap IS NOT NULL
		      UNION ALL
		      SELECT quote_asset AS asset_id, 1::numeric / vwap,
		             array_length(sources, 1),
		             bucket, 1
		        FROM prices_1m
		       WHERE base_asset IN ('native', 'CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA')
		         AND bucket >= now() - INTERVAL '7 days'
		         AND vwap > 0
		    ) u
		   ORDER BY asset_id, inverted, bucket DESC
		),
		asset_vs_xlm_1h AS (
		  -- Both directions, base-side preferred — see asset_vs_xlm.
		  SELECT DISTINCT ON (asset_id) asset_id, vwap
		    FROM (
		      SELECT base_asset AS asset_id, vwap, bucket, 0 AS inverted
		        FROM prices_1m
		       WHERE quote_asset IN ('native', 'CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA')
		         AND bucket BETWEEN now() - INTERVAL '90 minutes'
		                   AND now() - INTERVAL '55 minutes'
		         AND vwap IS NOT NULL
		      UNION ALL
		      SELECT quote_asset AS asset_id, 1::numeric / vwap, bucket, 1
		        FROM prices_1m
		       WHERE base_asset IN ('native', 'CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA')
		         AND bucket BETWEEN now() - INTERVAL '90 minutes'
		                   AND now() - INTERVAL '55 minutes'
		         AND vwap > 0
		    ) u
		   ORDER BY asset_id, inverted, bucket DESC
		),
		asset_vs_xlm_24h AS (
		  -- Both directions, base-side preferred — see asset_vs_xlm.
		  SELECT DISTINCT ON (asset_id) asset_id, vwap
		    FROM (
		      SELECT base_asset AS asset_id, vwap, bucket, 0 AS inverted
		        FROM prices_1m
		       WHERE quote_asset IN ('native', 'CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA')
		         AND bucket BETWEEN now() - INTERVAL '26 hours'
		                   AND now() - INTERVAL '23 hours 30 minutes'
		         AND vwap IS NOT NULL
		      UNION ALL
		      SELECT quote_asset AS asset_id, 1::numeric / vwap, bucket, 1
		        FROM prices_1m
		       WHERE base_asset IN ('native', 'CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA')
		         AND bucket BETWEEN now() - INTERVAL '26 hours'
		                   AND now() - INTERVAL '23 hours 30 minutes'
		         AND vwap > 0
		    ) u
		   ORDER BY asset_id, inverted, bucket DESC
		),
		asset_vs_xlm_7d AS (
		  -- Both directions, base-side preferred — see asset_vs_xlm.
		  SELECT DISTINCT ON (asset_id) asset_id, vwap
		    FROM (
		      SELECT base_asset AS asset_id, vwap, bucket, 0 AS inverted
		        FROM prices_1m
		       WHERE quote_asset IN ('native', 'CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA')
		         AND bucket BETWEEN now() - INTERVAL '7 days 12 hours'
		                   AND now() - INTERVAL '6 days 22 hours'
		         AND vwap IS NOT NULL
		      UNION ALL
		      SELECT quote_asset AS asset_id, 1::numeric / vwap, bucket, 1
		        FROM prices_1m
		       WHERE base_asset IN ('native', 'CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA')
		         AND bucket BETWEEN now() - INTERVAL '7 days 12 hours'
		                   AND now() - INTERVAL '6 days 22 hours'
		         AND vwap > 0
		    ) u
		   ORDER BY asset_id, inverted, bucket DESC
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
`

// refreshAssetPriceSnapshotUpsert recomputes every priced asset's
// headline price + 1h/24h/7d change + backing source count from
// [assetPriceCTEs] and upserts one row per asset into
// asset_price_snapshot.
//
// Spine: the assets that CAN have a price — `direct_usd ∪ asset_vs_xlm
// ∪ {native}` — not the 198k-row catalogue. An asset is priced by the
// COALESCE chain only if it has a 7-day-latest row in one of those two
// CTEs (the 1h/24h/7d lookbacks feed the CHANGE columns, never the
// price), so this spine is exactly the set that can produce a non-NULL
// price and nothing is lost by not scanning the catalogue. ~10.5k rows
// on r1 2026-09-02 (813 direct + 9,886 XLM-quoted).
//
// Money stays NUMERIC end to end (ADR-0003): `price_usd` and the three
// change columns are stored unrounded and unformatted, and the listing
// applies the identical `ROUND(…, 10)::text` / `to_char(…,
// 'FM999999990.00')` wire rendering it always applied — so the bytes on
// the wire are the bytes the inline derivation produced. Never a float.
//
// `computed_at` is stamped to the transaction timestamp so the sibling
// prune can drop assets whose price lapsed this pass, exactly as
// refreshAssetVolumeUpsert does.
const refreshAssetPriceSnapshotUpsert = `
INSERT INTO asset_price_snapshot AS s
       (asset_id, price_usd, change_1h_pct, change_24h_pct, change_7d_pct,
        source_count, computed_at)
WITH ` + assetPriceCTEs + `,
		priced_assets AS (
		  -- Every asset the COALESCE chain can price, and only those.
		  SELECT asset_id FROM direct_usd
		  UNION
		  SELECT asset_id FROM asset_vs_xlm
		  UNION
		  -- Native XLM is priced from the xlm_usd scalar, so it appears
		  -- in neither CTE above (there is no (native, native) row) and
		  -- has to be seeded. It falls back out below if xlm_usd and the
		  -- direct XLM/USDC row are both empty.
		  SELECT 'native'::text
		),
		derived AS (
		  SELECT
		    ca.asset_id,
		    ` + snapshotPriceUSDExpr + ` AS price_usd,
		    CASE
		      WHEN ca.asset_id = 'native'
		           AND (SELECT vwap FROM xlm_usd) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_1h) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_1h) > 0
		      THEN ((SELECT vwap FROM xlm_usd)
		          / (SELECT vwap FROM xlm_usd_1h) - 1) * 100
		      WHEN direct.vwap IS NOT NULL AND direct_1h.vwap IS NOT NULL
		           AND direct_1h.vwap > 0
		      THEN (direct.vwap / direct_1h.vwap - 1) * 100
		      WHEN vs_xlm.vwap IS NOT NULL AND vs_xlm_1h.vwap IS NOT NULL
		           AND vs_xlm_1h.vwap > 0
		           AND (SELECT vwap FROM xlm_usd) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_1h) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_1h) > 0
		      THEN ((vs_xlm.vwap    * (SELECT vwap FROM xlm_usd))
		          / (vs_xlm_1h.vwap * (SELECT vwap FROM xlm_usd_1h))
		          - 1) * 100
		      ELSE NULL
		    END AS change_1h_pct,
		    CASE
		      WHEN ca.asset_id = 'native'
		           AND (SELECT vwap FROM xlm_usd) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_24h) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_24h) > 0
		      THEN ((SELECT vwap FROM xlm_usd)
		          / (SELECT vwap FROM xlm_usd_24h) - 1) * 100
		      WHEN direct.vwap IS NOT NULL AND direct_24h.vwap IS NOT NULL
		           AND direct_24h.vwap > 0
		      THEN (direct.vwap / direct_24h.vwap - 1) * 100
		      WHEN vs_xlm.vwap IS NOT NULL AND vs_xlm_24h.vwap IS NOT NULL
		           AND vs_xlm_24h.vwap > 0
		           AND (SELECT vwap FROM xlm_usd) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_24h) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_24h) > 0
		      THEN ((vs_xlm.vwap     * (SELECT vwap FROM xlm_usd))
		          / (vs_xlm_24h.vwap * (SELECT vwap FROM xlm_usd_24h))
		          - 1) * 100
		      ELSE NULL
		    END AS change_24h_pct,
		    CASE
		      WHEN ca.asset_id = 'native'
		           AND (SELECT vwap FROM xlm_usd) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_7d) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_7d) > 0
		      THEN ((SELECT vwap FROM xlm_usd)
		          / (SELECT vwap FROM xlm_usd_7d) - 1) * 100
		      WHEN direct.vwap IS NOT NULL AND direct_7d.vwap IS NOT NULL
		           AND direct_7d.vwap > 0
		      THEN (direct.vwap / direct_7d.vwap - 1) * 100
		      WHEN vs_xlm.vwap IS NOT NULL AND vs_xlm_7d.vwap IS NOT NULL
		           AND vs_xlm_7d.vwap > 0
		           AND (SELECT vwap FROM xlm_usd) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_7d) IS NOT NULL
		           AND (SELECT vwap FROM xlm_usd_7d) > 0
		      THEN ((vs_xlm.vwap    * (SELECT vwap FROM xlm_usd))
		          / (vs_xlm_7d.vwap * (SELECT vwap FROM xlm_usd_7d))
		          - 1) * 100
		      ELSE NULL
		    END AS change_7d_pct,
		    -- Distinct venues backing price_usd (the latest per-asset
		    -- bucket's prices_1m.sources) — the liquidity signal the
		    -- API's market-cap valuation guard reads. NULL for native XLM
		    -- (triangulated price, always liquid).
		    CASE WHEN ca.asset_id = 'native' THEN NULL::int
		         ELSE COALESCE(direct.source_count, vs_xlm.source_count)
		    END AS source_count
		  FROM priced_assets ca
		  LEFT JOIN direct_usd        direct      ON direct.asset_id     = ca.asset_id
		  LEFT JOIN direct_usd_1h     direct_1h   ON direct_1h.asset_id  = ca.asset_id
		  LEFT JOIN direct_usd_24h    direct_24h  ON direct_24h.asset_id = ca.asset_id
		  LEFT JOIN direct_usd_7d     direct_7d   ON direct_7d.asset_id  = ca.asset_id
		  LEFT JOIN asset_vs_xlm      vs_xlm      ON vs_xlm.asset_id     = ca.asset_id
		  LEFT JOIN asset_vs_xlm_1h   vs_xlm_1h   ON vs_xlm_1h.asset_id  = ca.asset_id
		  LEFT JOIN asset_vs_xlm_24h  vs_xlm_24h  ON vs_xlm_24h.asset_id = ca.asset_id
		  LEFT JOIN asset_vs_xlm_7d   vs_xlm_7d   ON vs_xlm_7d.asset_id  = ca.asset_id
		)
		SELECT asset_id, price_usd, change_1h_pct, change_24h_pct,
		       change_7d_pct, source_count, now()
		  FROM derived
		 WHERE price_usd IS NOT NULL
ON CONFLICT (asset_id) DO UPDATE
   SET price_usd      = EXCLUDED.price_usd,
       change_1h_pct  = EXCLUDED.change_1h_pct,
       change_24h_pct = EXCLUDED.change_24h_pct,
       change_7d_pct  = EXCLUDED.change_7d_pct,
       source_count   = EXCLUDED.source_count,
       computed_at    = EXCLUDED.computed_at`

// refreshAssetPriceSnapshotPrune deletes assets that stopped being
// priceable this pass (nothing re-wrote them, so their computed_at
// stayed at the previous run). Same one-transaction now() trick as
// refreshAssetVolumePrune: just-upserted rows carry computed_at = now()
// and survive; lapsed rows carry an older timestamp and are dropped, so
// a delisted asset's last known price cannot linger as if it were
// current.
const refreshAssetPriceSnapshotPrune = `DELETE FROM asset_price_snapshot WHERE computed_at < now()`

// RefreshAssetListingRollups recomputes BOTH rollups the /v1/assets
// listing LEFT JOINs — asset_volume_24h (migration 0087, #43) and
// asset_price_snapshot (migration 0154, #331 F1) — and atomically
// replaces their contents.
//
// One transaction, on purpose: the two rollups are joined onto the same
// spine row and read together, so committing them together means a
// listing row's volume and its price are always from the same pass. It
// also keeps the lock posture of the original — upsert + prune take
// row-level locks only, never the ACCESS EXCLUSIVE that would stall
// concurrent reads of a customer-facing endpoint.
//
// Called on the aggregator's asset-rollup cadence
// (assetvolrollup.DefaultInterval, 2 min) via [Store.RefreshAssetVolume24h].
func (s *Store) RefreshAssetListingRollups(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("timescale: RefreshAssetListingRollups begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Bound this transaction's footprint, in the spirit of the sibling
	// character rollup (asset_volume_character_rollup.go): it now does
	// roughly 10x the work it did before the price half joined it, on a
	// connection that shares the primary with the customer-facing API.
	//
	// work_mem — the 7-day asset_vs_xlm DISTINCT ON sorts ~48k rows and
	// does not fit r1's 32 MB session default: measured 2026-09-02 it
	// spilled `external merge Disk: 51,072 kB` EVERY pass, which at a
	// 2-minute cadence is ~37 GB/day of temp write+read. 96 MB removes
	// the spill (64 MB does not) and takes the refresh from 1,765-2,021
	// ms to 1,316-1,432 ms. It is a per-sort-NODE ceiling, not an
	// allocation; observed peak across the whole plan is ~67 MB on one
	// background connection every 2 minutes.
	//
	// statement_timeout — a wedge guard, not a budget. The pass measures
	// ~1.6 s end to end (150 ms volume + ~1.4 s price), so 10 minutes is
	// ~375x headroom and can only fire on something pathological; when it
	// does, the transaction aborts, BOTH rollups keep their last good
	// contents, and the worker retries on its next tick — strictly better
	// than holding row locks on two rollups the listing reads.
	//
	// SET LOCAL, so both revert on COMMIT/ROLLBACK and cannot leak onto
	// the pooled connection.
	if _, err := tx.ExecContext(ctx, "SET LOCAL work_mem = '96MB'"); err != nil {
		return fmt.Errorf("timescale: RefreshAssetListingRollups set work_mem: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "SET LOCAL statement_timeout = '10min'"); err != nil {
		return fmt.Errorf("timescale: RefreshAssetListingRollups set timeout: %w", err)
	}

	if _, err := tx.ExecContext(ctx, refreshAssetVolumeUpsert); err != nil {
		return fmt.Errorf("timescale: RefreshAssetListingRollups volume upsert: %w", err)
	}
	if _, err := tx.ExecContext(ctx, refreshAssetVolumePrune); err != nil {
		return fmt.Errorf("timescale: RefreshAssetListingRollups volume prune: %w", err)
	}
	if _, err := tx.ExecContext(ctx, refreshAssetPriceSnapshotUpsert); err != nil {
		return fmt.Errorf("timescale: RefreshAssetListingRollups price upsert: %w", err)
	}
	if _, err := tx.ExecContext(ctx, refreshAssetPriceSnapshotPrune); err != nil {
		return fmt.Errorf("timescale: RefreshAssetListingRollups price prune: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("timescale: RefreshAssetListingRollups commit: %w", err)
	}
	return nil
}
