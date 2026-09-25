// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
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
// So the derivation ([assetPriceCTEs]) runs here instead, off the
// request path, and the aggregator folds it into one small keyed-on-PK
// table on the same cadence the sibling volume rollup already runs at.
// The listing then LEFT JOINs `asset_price_snapshot`.
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
// Staleness contract — two different ages, bounded differently.
//
// ROLLUP age, how long ago the served row was computed:
//
//	prices_1m CAGG lag      <= ~90 s  (30 s schedule + 30 s end_offset,
//	                                   migration 0002)
//	rollup refresh cadence   = 2 min  (assetvolrollup.DefaultInterval)
//	API listing cache TTL    = 2 min  (v1.NewCachedAssetsReader)
//	-------------------------------------------------------------
//	worst-case rollup age   ~= 5.5 min
//
// [assetPriceSnapshotMaxAge] is a HARD ceiling on this age, spliced into
// the listing's join, so a wedged or dead aggregator can never serve
// indefinitely-old prices: past it the join misses, the asset renders
// as an asset with no price (`price_usd` absent, rank tier 1), and the
// aggregator's own per-binary heartbeat is what pages.
//
// OBSERVATION age, how old the trades behind the price are, is NOT
// bounded by that ceiling and is not stored. The price comes from the
// newest traded minute across every arm and direction
// ([priceArmPickExpr]), which for an asset that trades rarely can be up
// to the 7-day lookback old. Choosing by recency is what stops a
// days-old USD print masking a live XLM market; a hard age cutoff would
// instead blank every weekly-trading asset's price, which is a product
// decision this rollup does not make.
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

// priceArmPickExpr names the arm that prices an asset: 'native' (XLM
// itself, from the xlm_usd scalar), 'direct' (a USD-proxy market) or
// 'xlm' (an XLM market triangulated through xlm_usd); NULL when none can.
//
// The arm with the NEWER observation wins. Choosing by arm order made
// any USD print in the 7-day lookback beat an XLM market trading this
// minute, so a days-old price was served, and fed market cap, as current.
// A triangulated observation is as old as the older of its two legs; on
// a tie the direct arm wins, as it has no triangulation error. Aliases
// are those of [priceArmJoins].
const priceArmPickExpr = `CASE
		      WHEN ca.asset_id = 'native' AND (SELECT vwap FROM xlm_usd) IS NOT NULL
		        THEN 'native'
		      WHEN direct.vwap IS NOT NULL
		           AND (vs_xlm.vwap IS NULL
		                OR (SELECT vwap FROM xlm_usd) IS NULL
		                OR direct.bucket >= LEAST(vs_xlm.bucket, (SELECT bucket FROM xlm_usd)))
		        THEN 'direct'
		      WHEN vs_xlm.vwap IS NOT NULL AND (SELECT vwap FROM xlm_usd) IS NOT NULL
		        THEN 'xlm'
		    END`

// priceArmJoins attaches every arm CTE of [assetPriceArmCTEs] to the
// spine row `ca`, then picks its arm as `pick.arm`. Shared verbatim by
// the rollup and the detail query so the two cannot choose differently.
const priceArmJoins = `
		  LEFT JOIN direct_usd        direct      ON direct.asset_id     = ca.asset_id
		  LEFT JOIN direct_usd_1h     direct_1h   ON direct_1h.asset_id  = ca.asset_id
		  LEFT JOIN direct_usd_24h    direct_24h  ON direct_24h.asset_id = ca.asset_id
		  LEFT JOIN direct_usd_7d     direct_7d   ON direct_7d.asset_id  = ca.asset_id
		  LEFT JOIN asset_vs_xlm      vs_xlm      ON vs_xlm.asset_id     = ca.asset_id
		  LEFT JOIN asset_vs_xlm_1h   vs_xlm_1h   ON vs_xlm_1h.asset_id  = ca.asset_id
		  LEFT JOIN asset_vs_xlm_24h  vs_xlm_24h  ON vs_xlm_24h.asset_id = ca.asset_id
		  LEFT JOIN asset_vs_xlm_7d   vs_xlm_7d   ON vs_xlm_7d.asset_id  = ca.asset_id
		  CROSS JOIN LATERAL (SELECT ` + priceArmPickExpr + ` AS arm) pick`

// snapshotPriceUSDExpr is the headline USD price, read through the arm
// [priceArmPickExpr] chose. The listing's `listingPriceUSDExpr` reads it
// back as `aps.price_usd`; the detail query renders it inline.
const snapshotPriceUSDExpr = `CASE pick.arm
		      WHEN 'native' THEN (SELECT vwap FROM xlm_usd)
		      WHEN 'direct' THEN direct.vwap
		      WHEN 'xlm'    THEN vs_xlm.vwap * (SELECT vwap FROM xlm_usd)
		    END`

// priceChangePctExpr is the unrounded percentage change over lookback
// ("1h", "24h" or "7d"), with both legs read through the arm that priced
// the asset. Mixing arms compared this minute's price with a lookback
// taken against a different, possibly days-stale, "now"; when the chosen
// arm has no lookback row the change is NULL.
func priceChangePctExpr(lookback string) string {
	return fmt.Sprintf(`CASE pick.arm
		      WHEN 'native' THEN CASE WHEN (SELECT vwap FROM xlm_usd_%[1]s) > 0
		        THEN ((SELECT vwap FROM xlm_usd) / (SELECT vwap FROM xlm_usd_%[1]s) - 1) * 100 END
		      WHEN 'direct' THEN CASE WHEN direct_%[1]s.vwap > 0
		        THEN (direct.vwap / direct_%[1]s.vwap - 1) * 100 END
		      WHEN 'xlm' THEN CASE WHEN vs_xlm_%[1]s.vwap > 0 AND (SELECT vwap FROM xlm_usd_%[1]s) > 0
		        THEN ((vs_xlm.vwap * (SELECT vwap FROM xlm_usd))
		            / (vs_xlm_%[1]s.vwap * (SELECT vwap FROM xlm_usd_%[1]s)) - 1) * 100 END
		    END`, lookback)
}

// snapshotNormalizedPriceUSDExpr is [snapshotPriceUSDExpr] with the
// dex-nonstandard-decimals forward normalisation applied, and it is the
// value the rollup STORES. `nda` is the refresh's LEFT JOIN onto
// nonstandard_decimals_assets (migration 0093).
//
// prices_1m holds RAW quote/base ratios of smallest-unit amounts, so for
// a token whose on-chain decimals() is not 7 every arm above is off by
// the same 10^(7 - decimals): the direct arm divides by a 7-decimals USD
// proxy, the XLM arm by 7-decimals XLM and then multiplies by a 7/7
// XLM/USD ratio, and a flipped-direction row's legs are the same token
// amounts read the other way round. One exact
// factor, 10^(decimals - 7), corrects whichever arm answered — the same
// single factor the API's catalogue reader applies
// (v1.Server.normalizeCatalogueUSD).
//
// Why the WRITER, when every other surface normalises at read time
// (prices_1m and change_summary_5m stay raw on purpose):
//
//   - No ratchet. /v1/changes normalises at read because its upsert
//     keeps ath_value / atl_value with GREATEST / LEAST, and a token is
//     flagged only after it has traded, so a write-side switch would pin
//     the extreme to a figure from the old scale for good. This rollup
//     has nothing of the kind: the upsert OVERWRITES every column, the
//     whole table is recomputed each pass, and the prune drops what was
//     not rewritten. A newly confirmed (or reconciled-away) row takes
//     full effect on the next 2-minute pass and leaves no residue.
//   - One place, every reader. The column is read by the listing spine,
//     by [Store.ContractCatalogueRows], and through them by every API
//     projection of a listing row (the /v1/assets listing phases, the
//     RWA classic and contract listings, the catalogue-twin merge and
//     the lake-supply prewarm). None of them normalised, and the RWA
//     contract listing multiplies this price by supply to publish a
//     market cap.
//   - Full precision. The column is unrounded NUMERIC, so the multiply
//     is exact and the listing's ROUND(price_usd, 10) now runs AFTER the
//     correction. Correcting at read would scale an already-rounded
//     string: an 18-decimals token worth 1 USD has a raw ratio of 1e-11,
//     which ROUND(…, 10) turns into zero before any reader sees it.
//
// A READER OF THIS COLUMN MUST NOT NORMALISE IT AGAIN. The three change
// columns need no factor: each is a ratio of two legs read through the
// same arm, so the scale cancels.
//
// A MULTIPLIER OF THIS COLUMN MUST USE THE SAME DECIMALS. Reading is not
// the only way to consume a scale: a market cap is this price times a
// smallest-unit supply divided by 10^decimals, and with a true-scale
// price that exponent has to be the token's real decimals. Against the
// RAW ratio the standard 7 was right by cancellation, so storing the
// corrected price moved the divisor's requirement with it. The shared
// listing fill takes it from this same table
// (v1.Server.applyConfirmedListingDecimals); the RWA contract arm's own
// fill divides by the lake's decimals() reading.
//
// The CASE (rather than a COALESCE'd factor of 1) keeps the stored value
// for every asset with no confirmed row the exact NUMERIC it always was,
// display scale included. power(numeric, numeric) with an integral
// exponent is exact in both directions, so money stays NUMERIC end to
// end (ADR-0003).
const snapshotNormalizedPriceUSDExpr = `CASE WHEN nda.decimals IS NULL
		         THEN ` + snapshotPriceUSDExpr + `
		         ELSE ` + snapshotPriceUSDExpr + `
		              * power(10::numeric, (nda.decimals - 7)::numeric)
		    END`

// Price-arm windows. The headline arms read the newest traded minute of
// the last 7 days; each change arm reads the newest minute within the
// documented tolerance of 1 h / 24 h / 7 d ago (±5 min / ±30 min / ±2 h,
// see AssetRow.Change1hPct), else the change is NULL. A wider window
// published a 1.5-hour move as change_1h_pct.
const (
	priceWindowNow = `bucket >= now() - INTERVAL '7 days'`
	priceWindow1h  = `bucket BETWEEN now() - INTERVAL '65 minutes' AND now() - INTERVAL '55 minutes'`
	priceWindow24h = `bucket BETWEEN now() - INTERVAL '24 hours 30 minutes' AND now() - INTERVAL '23 hours 30 minutes'`
	priceWindow7d  = `bucket BETWEEN now() - INTERVAL '7 days 2 hours' AND now() - INTERVAL '6 days 22 hours'`
)

// unionPriceArmCTE renders the CTE pair `<name>_rows` / `<name>` for one
// price arm. Per asset, `<name>` carries the newest 1-minute bucket in
// window in which the asset traded against any of quotes, in EITHER
// stored direction; the VWAP of the union of that bucket's rows; and the
// distinct venues behind them.
//
// prices_1m keeps a market in whichever direction its source wrote it
// (aquarius stores base = token_in; SDEX stores both), and `vwap` is
// always base priced in quote. So each row is re-expressed as two legs in
// the arm's (asset, quote) orientation and the leg sums are re-divided,
// the SQL form of [combineDirVWAP]:
//
//	(asset, q) row: asset leg = volume,         q leg = vwap × volume
//	(q, asset) row: asset leg = vwap × volume,  q leg = volume
//
// Reading one direction, or preferring one, priced an asset from
// whichever side of its book last traded in that orientation: days old,
// or one-sided, while the other side was live.
//
// asset is "" for every asset (the rollup) or a scalar SQL expression
// that pins the arm to one asset (the detail query).
func unionPriceArmCTE(name, quotes, window, asset string) string {
	var baseSide, flipped string
	if asset != "" {
		baseSide = " AND base_asset = " + asset
		flipped = " AND quote_asset = " + asset
	}
	return fmt.Sprintf(`
		%[1]s_rows AS (
		  SELECT asset_id, bucket, asset_leg, quote_leg, sources
		    FROM (
		      SELECT u.*, max(u.bucket) OVER (PARTITION BY u.asset_id) AS newest
		        FROM (
		          SELECT base_asset AS asset_id, bucket, volume AS asset_leg,
		                 vwap * volume AS quote_leg, sources
		            FROM prices_1m
		           WHERE quote_asset IN (%[2]s)%[4]s
		             AND %[3]s
		             AND vwap > 0 AND volume > 0
		          UNION ALL
		          SELECT quote_asset, bucket, vwap * volume, volume, sources
		            FROM prices_1m
		           WHERE base_asset IN (%[2]s)%[5]s
		             AND %[3]s
		             AND vwap > 0 AND volume > 0
		        ) u
		    ) r
		   WHERE bucket = newest
		),
		%[1]s AS (
		  SELECT v.asset_id, v.bucket, v.vwap, s.source_count
		    FROM (SELECT asset_id, max(bucket) AS bucket,
		                 sum(quote_leg) / sum(asset_leg) AS vwap
		            FROM %[1]s_rows
		           GROUP BY asset_id) v
		    LEFT JOIN (SELECT asset_id, count(DISTINCT src)::int AS source_count
		                 FROM %[1]s_rows CROSS JOIN LATERAL unnest(sources) AS src
		                GROUP BY asset_id) s ON s.asset_id = v.asset_id
		)`, name, quotes, window, baseSide, flipped)
}

// assetPriceArmCTEs renders the eight per-asset arms [priceArmJoins]
// reads, now and at each change lookback: `direct_usd*` against the
// USD proxies ([usdProxyQuotes]: fiat:USD, and USDC taken AS USD per the
// aggregator's stablecoin-proxy policy, in its classic and SAC forms
// because Soroban venues carry the SAC id) and `asset_vs_xlm*` against
// XLM in both identity forms ([xlmQuotes]). TestProxyQuoteLists_Lockstep
// pins both IN-lists.
func assetPriceArmCTEs(asset string) string {
	arms := make([]string, 0, 8)
	for _, arm := range []struct{ name, quotes string }{
		{"direct_usd", usdProxyQuotes},
		{"asset_vs_xlm", xlmQuotes},
	} {
		for _, lb := range []struct{ suffix, window string }{
			{"", priceWindowNow},
			{"_1h", priceWindow1h},
			{"_24h", priceWindow24h},
			{"_7d", priceWindow7d},
		} {
			arms = append(arms, unionPriceArmCTE(arm.name+lb.suffix, arm.quotes, lb.window, asset))
		}
	}
	return strings.Join(arms, ",")
}

// assetPriceCTEs is the rollup's price substrate: every asset's eight
// arms plus the XLM/USD scalars. The `/*PUSHDOWN_*/` markers the listing
// once carried are gone: a full all-asset recompute has nothing to
// narrow to (same reason refreshAssetVolumeUpsert carries none).
var assetPriceCTEs = assetPriceArmCTEs("") + "," + xlmUSDCTEs

// xlmUSDCTEs is XLM's own USD price now and at each change lookback,
// shared by the rollup and the detail query. `bucket` is the observation
// minute [priceArmPickExpr] ages a triangulated price by.
const xlmUSDCTEs = `
		xlm_usd AS (
		  -- prices_1m doesn't carry (native, fiat:USD) rows — XLM's
		  -- USD price is computed by the aggregator's triangulation
		  -- worker and lives in Redis, not the materialised view.
		  -- Mirror the aggregator's stablecoin-proxy policy in SQL
		  -- (AGENTS.md: "stablecoin fiat-proxy is aggregator policy"
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
		  SELECT vwap, bucket
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
		     AND bucket BETWEEN now() - INTERVAL '65 minutes'
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
		     AND bucket BETWEEN now() - INTERVAL '24 hours 30 minutes'
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
		     AND bucket BETWEEN now() - INTERVAL '7 days 2 hours'
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
// ∪ {native}` — not the 198k-row catalogue. An asset is priced only if
// it has a row in one of those two CTEs within the 7-day lookback (the
// 1h/24h/7d lookbacks feed the CHANGE columns, never the price), so this
// spine is exactly the set that can produce a non-NULL price and nothing
// is lost by not scanning the catalogue.
//
// Money stays NUMERIC end to end (ADR-0003): `price_usd` and the three
// change columns are stored unrounded and unformatted, and the listing
// applies the `ROUND(…, 10)::text` / `to_char(…, 'FM999999990.00')` wire
// rendering. Never a float. The decimals correction for a CONFIRMED
// non-7-decimals token is applied here, before anything is rounded — see
// [snapshotNormalizedPriceUSDExpr].
//
// `computed_at` is stamped to the transaction timestamp so the sibling
// prune can drop assets whose price lapsed this pass, exactly as
// refreshAssetVolumeUpsert does.
var refreshAssetPriceSnapshotUpsert = `
INSERT INTO asset_price_snapshot AS s
       (asset_id, price_usd, change_1h_pct, change_24h_pct, change_7d_pct,
        source_count, computed_at)
WITH ` + assetPriceCTEs + `,
		priced_assets AS (
		  -- Every asset an arm can price, and only those.
		  SELECT asset_id FROM direct_usd
		  UNION
		  SELECT asset_id FROM asset_vs_xlm
		  UNION
		  -- Native XLM is priced from the xlm_usd scalar, which is not a
		  -- per-asset arm, so it is seeded. It falls back out below if
		  -- xlm_usd and its own direct row are both empty.
		  SELECT 'native'::text
		),
		derived AS (
		  SELECT
		    ca.asset_id,
		    ` + snapshotNormalizedPriceUSDExpr + ` AS price_usd,
		    ` + priceChangePctExpr("1h") + ` AS change_1h_pct,
		    ` + priceChangePctExpr("24h") + ` AS change_24h_pct,
		    ` + priceChangePctExpr("7d") + ` AS change_7d_pct,
		    -- Distinct venues behind price_usd (every row of the minute the
		    -- chosen arm priced from) — the liquidity signal the API's
		    -- market-cap valuation guard reads. NULL for native XLM
		    -- (triangulated price, always liquid).
		    CASE WHEN ca.asset_id = 'native' THEN NULL::int
		         WHEN pick.arm = 'direct' THEN direct.source_count
		         WHEN pick.arm = 'xlm' THEN vs_xlm.source_count
		    END AS source_count
		  FROM priced_assets ca` + priceArmJoins + `
		  -- Confirmed non-7-decimals tokens only; see
		  -- snapshotNormalizedPriceUSDExpr. The table is tiny (near-zero
		  -- confirmed offenders) and keyed on its primary key.
		  LEFT JOIN nonstandard_decimals_assets nda ON nda.asset = ca.asset_id
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

// refreshAssetPriceSnapshotPruneExpired is the zero-row pass's prune (see
// [pruneRollup]): past assetPriceSnapshotMaxAge the listing join already
// ignores a row, so nothing older is worth keeping.
const refreshAssetPriceSnapshotPruneExpired = `DELETE FROM asset_price_snapshot WHERE computed_at < now() - INTERVAL '` + assetPriceSnapshotMaxAge + `'`

// pruneRollup drops the rows this pass did not re-write. An upsert that
// wrote NOTHING is an upstream fault (prices_1m empty, rebuilt WITH NO
// DATA, its refresh stalled), not evidence that every asset lapsed at
// once, so it must not empty the served rollup: it runs pruneExpired,
// which drops only rows already past the rollup's own validity bound.
func pruneRollup(ctx context.Context, tx *sql.Tx, upserted int64, prune, pruneExpired string) error {
	q := prune
	if upserted == 0 {
		q = pruneExpired
	}
	_, err := tx.ExecContext(ctx, q)
	return err
}

// execRowCount runs an upsert and returns how many rows it wrote.
func execRowCount(ctx context.Context, tx *sql.Tx, q string) (int64, error) {
	res, err := tx.ExecContext(ctx, q)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

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

	volN, err := execRowCount(ctx, tx, refreshAssetVolumeUpsert)
	if err != nil {
		return fmt.Errorf("timescale: RefreshAssetListingRollups volume upsert: %w", err)
	}
	if err := pruneRollup(ctx, tx, volN, refreshAssetVolumePrune, refreshAssetVolumePruneExpired); err != nil {
		return fmt.Errorf("timescale: RefreshAssetListingRollups volume prune: %w", err)
	}
	priceN, err := execRowCount(ctx, tx, refreshAssetPriceSnapshotUpsert)
	if err != nil {
		return fmt.Errorf("timescale: RefreshAssetListingRollups price upsert: %w", err)
	}
	if err := pruneRollup(ctx, tx, priceN, refreshAssetPriceSnapshotPrune, refreshAssetPriceSnapshotPruneExpired); err != nil {
		return fmt.Errorf("timescale: RefreshAssetListingRollups price prune: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("timescale: RefreshAssetListingRollups commit: %w", err)
	}
	return nil
}
