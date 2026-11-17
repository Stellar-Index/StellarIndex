// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// asset_price_snapshot: the per-asset headline-price rollup behind the GET /v1/assets listing.
// Deriving it per request costs a dozen `DISTINCT ON ... FROM prices_1m` CTEs per asset, so
// [assetPriceCTEs] runs off the request path and the listing LEFT JOINs the result. Not a CAGG (no
// GROUP BY expresses it) and not a materialised view (REFRESH takes ACCESS EXCLUSIVE and would
// stall the listing; upsert+prune takes row locks only).
//
// Rollup age is capped by [assetPriceSnapshotMaxAge] in the listing's join, so a dead aggregator
// renders assets unpriced rather than stale. Observation age is not capped: the price is the newest
// traded minute within the 7-day lookback, preferring one over the $0.01 notional floor ([priceArmPickExpr]); a hard cutoff would blank
// every weekly-trading asset. The listing row can lag its detail page by up to that ceiling.

// assetPriceSnapshotMaxAge is how old a snapshot row may be and still be served (enforced in the listing's LEFT JOIN, see
// [listAssetsBaseSelect]): 7 missed 2-minute passes, so a restart never blanks the page.
const assetPriceSnapshotMaxAge = "15 minutes"

// priceArmPickExpr names the pricing arm: 'native' (xlm_usd anchor), 'direct' (USD-proxy market) or 'xlm' (XLM market via anchor `ax`
// at its own minute). An arm whose minute cleared the $0.01 notional floor beats one that did not, whatever their age (a sub-cent
// print costs nothing to place); between arms of equal floor status the newer observation wins and ties go direct. No anchor within
// [xlmUSDAnchorMaxAge] means the xlm arm cannot price.
const priceArmPickExpr = `CASE
		      WHEN ca.asset_id = 'native'
		        THEN CASE WHEN (SELECT vwap FROM xlm_usd) IS NOT NULL THEN 'native' END
		      WHEN direct.vwap IS NOT NULL
		           AND (ax.vwap IS NULL OR (direct.floored, direct.bucket) >= (vs_xlm.floored, vs_xlm.bucket))
		        THEN 'direct'
		      WHEN ax.vwap IS NOT NULL
		        THEN 'xlm'
		    END`

// priceArmJoins attaches every [assetPriceArmCTEs] arm to spine row `ca` plus the anchor at each arm's minute (`ax*`), picking `pick.arm`.
// Shared by the rollup and the detail query so they cannot choose differently.
func priceArmJoins(anchorJoin func(alias, ts string) string) string {
	return `
		  LEFT JOIN direct_usd        direct      ON direct.asset_id     = ca.asset_id
		  LEFT JOIN direct_usd_1h     direct_1h   ON direct_1h.asset_id  = ca.asset_id
		  LEFT JOIN direct_usd_24h    direct_24h  ON direct_24h.asset_id = ca.asset_id
		  LEFT JOIN direct_usd_7d     direct_7d   ON direct_7d.asset_id  = ca.asset_id
		  LEFT JOIN asset_vs_xlm      vs_xlm      ON vs_xlm.asset_id     = ca.asset_id
		  LEFT JOIN asset_vs_xlm_1h   vs_xlm_1h   ON vs_xlm_1h.asset_id  = ca.asset_id
		  LEFT JOIN asset_vs_xlm_24h  vs_xlm_24h  ON vs_xlm_24h.asset_id = ca.asset_id
		  LEFT JOIN asset_vs_xlm_7d   vs_xlm_7d   ON vs_xlm_7d.asset_id  = ca.asset_id
		  ` + anchorJoin("ax", "vs_xlm.bucket") + `
		  ` + anchorJoin("ax_1h", "vs_xlm_1h.bucket") + `
		  ` + anchorJoin("ax_24h", "vs_xlm_24h.bucket") + `
		  ` + anchorJoin("ax_7d", "vs_xlm_7d.bucket") + `
		  CROSS JOIN LATERAL (SELECT ` + priceArmPickExpr + ` AS arm) pick`
}

// xlmUSDGridJoin joins the rollup's anchor grid ([xlmUSDAnchorGridCTE]).
func xlmUSDGridJoin(alias, ts string) string {
	return "LEFT JOIN xlm_usd_grid " + alias + " ON " + alias + ".minute = " + ts
}

// xlmUSDLateralJoin returns a join rendering [xlmUSDAnchorAt] per row, for
// single-asset queries where a grid scan would cost more than four probes.
func xlmUSDLateralJoin(sac string) func(alias, ts string) string {
	return func(alias, ts string) string {
		return "LEFT JOIN LATERAL (" + xlmUSDAnchorAt(ts, sac) + ") " + alias + " ON true"
	}
}

// xlmUSDGridMissing is true when [xlmUSDGridJoin] would leave the anchor at ts NULL; NOT IN over the uncorrelated grid hashes once.
func xlmUSDGridMissing(ts string) string {
	return "(" + ts + " NOT IN (SELECT minute FROM xlm_usd_grid WHERE vwap IS NOT NULL))"
}

// xlmUSDLateralMissing is true when [xlmUSDLateralJoin] would leave the anchor at ts NULL; the SAC is the detail query's $2.
func xlmUSDLateralMissing(ts string) string {
	return "NOT EXISTS (SELECT 1 FROM (" + xlmUSDAnchorAt(ts, "$2::text") + ") fa WHERE fa.vwap IS NOT NULL)"
}

// snapshotPriceUSDExpr is the headline USD price through the arm [priceArmPickExpr] chose (read back as `aps.price_usd`).
const snapshotPriceUSDExpr = `CASE pick.arm
		      WHEN 'native' THEN (SELECT vwap FROM xlm_usd)
		      WHEN 'direct' THEN direct.vwap
		      WHEN 'xlm'    THEN vs_xlm.vwap * ax.vwap
		    END`

// priceChangePctExpr is the unrounded change over lookback ("1h", "24h", "7d"), both legs through the arm that priced the asset
// (mixing arms would compare against a possibly stale "now"); NULL when that arm has no lookback row. Also NULL unless the lookback
// minute is strictly older than the headline minute: the headline may fall back days to a floored trade, the lookback does not. NULL
// too when the headline cleared the notional floor and the lookback did not: a sub-cent minute is no baseline for a real price.
func priceChangePctExpr(lookback string) string {
	return fmt.Sprintf(`CASE pick.arm
		      WHEN 'native' THEN CASE WHEN (SELECT vwap FROM xlm_usd_%[1]s) > 0
		          AND (SELECT bucket FROM xlm_usd_%[1]s) < (SELECT bucket FROM xlm_usd)
		        THEN ((SELECT vwap FROM xlm_usd) / (SELECT vwap FROM xlm_usd_%[1]s) - 1) * 100 END
		      WHEN 'direct' THEN CASE WHEN direct_%[1]s.vwap > 0 AND direct_%[1]s.bucket < direct.bucket
		          AND (direct_%[1]s.floored OR NOT direct.floored)
		        THEN (direct.vwap / direct_%[1]s.vwap - 1) * 100 END
		      WHEN 'xlm' THEN CASE WHEN vs_xlm_%[1]s.vwap > 0 AND ax_%[1]s.vwap > 0 AND vs_xlm_%[1]s.bucket < vs_xlm.bucket
		          AND (vs_xlm_%[1]s.floored OR NOT vs_xlm.floored)
		        THEN ((vs_xlm.vwap * ax.vwap)
		            / (vs_xlm_%[1]s.vwap * ax_%[1]s.vwap) - 1) * 100 END
		    END`, lookback)
}

// snapshotNormalizedPriceUSDExpr is [snapshotPriceUSDExpr] corrected for non-7 decimals (`nda` joins nonstandard_decimals_assets); it
// is what the rollup stores, so readers must not normalise again (market cap uses the real decimals). The CASE keeps unconfirmed assets
// byte-identical; integral power(numeric, numeric) is exact (ADR-0003).
const snapshotNormalizedPriceUSDExpr = `CASE WHEN nda.decimals IS NULL
		         THEN ` + snapshotPriceUSDExpr + `
		         ELSE ` + snapshotPriceUSDExpr + `
		              * power(10::numeric, (nda.decimals - 7)::numeric)
		    END`

// Price-arm windows. The headline arms read the newest traded minute of the last 7 days; each
// change arm reads the newest minute within the tolerance of 1 h / 24 h / 7 d ago (+-5 min /
// +-30 min / +-2 h, see AssetRow.Change1hPct), else NULL. Wider would publish a 1.5-hour move as
// change_1h_pct.
const (
	priceWindow1hLo  = `now() - INTERVAL '65 minutes'`
	priceWindow1hHi  = `now() - INTERVAL '55 minutes'`
	priceWindow24hLo = `now() - INTERVAL '24 hours 30 minutes'`
	priceWindow24hHi = `now() - INTERVAL '23 hours 30 minutes'`
	priceWindow7dLo  = `now() - INTERVAL '7 days 2 hours'`
	priceWindow7dHi  = `now() - INTERVAL '6 days 22 hours'`

	priceWindowNow = `bucket >= now() - INTERVAL '7 days'`
	priceWindow1h  = `bucket BETWEEN ` + priceWindow1hLo + ` AND ` + priceWindow1hHi
	priceWindow24h = `bucket BETWEEN ` + priceWindow24hLo + ` AND ` + priceWindow24hHi
	priceWindow7d  = `bucket BETWEEN ` + priceWindow7dLo + ` AND ` + priceWindow7dHi
)

// unionPriceArmCTE renders CTE pair `<name>_rows`/`<name>`: per asset, the newest 1m bucket in window against any of quotes in either
// direction, its VWAP and venues. Rows are re-expressed as two legs and re-divided (see [combineDirVWAP]), weighted by volume_priced
// (vwap covers only both-leg-positive trades). asset "" means all assets; floored is the arm's floor flag over prices_1m row `p`.
func unionPriceArmCTE(name, quotes, window, asset, floored string) string {
	var baseSide, flipped string
	if asset != "" {
		baseSide = " AND base_asset = " + asset
		flipped = " AND quote_asset = " + asset
	}
	return fmt.Sprintf(`
		%[1]s_rows AS (
		  SELECT asset_id, bucket, asset_leg, quote_leg, sources, floored
		    FROM (
		      -- vwap carries no notional floor, so prefer the newest minute holding a trade over $0.01 and fall back to the newest
		      -- minute only when the window has none; within the chosen minute a sub-cent direction is dropped too.
		      SELECT u.*,
		             COALESCE(max(u.bucket) FILTER (WHERE u.floored) OVER w, max(u.bucket) OVER w) AS newest,
		             bool_or(u.floored) OVER w AS any_floored
		        FROM (
		          SELECT base_asset AS asset_id, bucket, volume_priced AS asset_leg,
		                 vwap * volume_priced AS quote_leg, sources, %[6]s AS floored
		            FROM prices_1m p
		           WHERE quote_asset IN (%[2]s)%[4]s
		             AND %[3]s
		             AND vwap > 0 AND volume_priced > 0
		          UNION ALL
		          SELECT quote_asset, bucket, vwap * volume_priced, volume_priced, sources, %[6]s AS floored
		            FROM prices_1m p
		           WHERE base_asset IN (%[2]s)%[5]s
		             AND %[3]s
		             AND vwap > 0 AND volume_priced > 0
		        ) u
		      WINDOW w AS (PARTITION BY u.asset_id)
		    ) r
		   WHERE bucket = newest AND (floored OR NOT any_floored)
		),
		%[1]s AS (
		  SELECT v.asset_id, v.bucket, v.vwap, v.floored, s.source_count
		    FROM (SELECT asset_id, max(bucket) AS bucket,
		                 sum(quote_leg) / sum(asset_leg) AS vwap,
		                 bool_or(floored) AS floored
		            FROM %[1]s_rows
		           GROUP BY asset_id) v
		    LEFT JOIN (SELECT asset_id, count(DISTINCT src)::int AS source_count
		                 FROM %[1]s_rows CROSS JOIN LATERAL unnest(sources) AS src
		                GROUP BY asset_id) s ON s.asset_id = v.asset_id
		)`, name, quotes, window, baseSide, flipped, floored)
}

// xlmArmFlooredSQL is the XLM arm's floor flag: an unstamped minute (volume_usd = 0) is floored only while its anchor is missing at read
// time too, so ingest and read agree it is an anchor gap; with an anchor present it may be a crumb, so it ranks as one.
func xlmArmFlooredSQL(anchorMissing func(ts string) string) string {
	return `CASE WHEN p.notional_trade_count > 0 THEN true
		                      WHEN p.volume_usd = 0 THEN ` + anchorMissing("p.bucket") + `
		                      ELSE false END`
}

// assetPriceArmCTEs renders the eight arms [priceArmJoins] reads: `direct_usd*` against [usdProxyQuotes], `asset_vs_xlm*` against
// [xlmQuotes]. TestProxyQuoteLists_Lockstep pins both IN-lists; xlmQuoteList is an [xlmQuotesBound] form whose placeholder the caller
// binds to the native SAC. anchorMissing must agree with the anchor join the caller hands [priceArmJoins].
func assetPriceArmCTEs(asset, xlmQuoteList string, anchorMissing func(ts string) string) string {
	arms := make([]string, 0, 8)
	// A direct USD minute stamped 0 means missing peg config or dust that rounds to zero, never an anchor gap: it is a crumb.
	for _, arm := range []struct{ name, quotes, floored string }{
		{"direct_usd", usdProxyQuotes, notionalMinuteSQL},
		{"asset_vs_xlm", xlmQuoteList, xlmArmFlooredSQL(anchorMissing)},
	} {
		for _, lb := range []struct{ suffix, window string }{
			{"", priceWindowNow},
			{"_1h", priceWindow1h},
			{"_24h", priceWindow24h},
			{"_7d", priceWindow7d},
		} {
			arms = append(arms, unionPriceArmCTE(arm.name+lb.suffix, arm.quotes, lb.window, asset, arm.floored))
		}
	}
	return strings.Join(arms, ",")
}

// assetPriceCTEs is the rollup's price substrate (anchor grid, eight arms, XLM scalars); no pushdown markers since the recompute is
// all-asset. The grid comes first because the XLM arms read it. $1 is canonical.NativeSACContractID().
var assetPriceCTEs = xlmUSDAnchorGridCTE("xlm_usd_grid", priceWindow7dLo, nativeSACParam(1)) + "," +
	assetPriceArmCTEs("", xlmQuotesBound1, xlmUSDGridMissing) + "," +
	xlmUSDNativeCTEs(nativeSACParam(1))

// notionalMinuteSQL is true for a prices_1m row holding a trade over the $0.01 notional floor. volume_usd = 0 means no trade in the
// minute was USD-stamped (no anchor at ingest; nothing restamps it) or every stamp rendered 0 at FloatString(8): unpriced or dust, never
// proof of a real trade. Series picks rank on this alone since they price XLM per period, not at the minute.
const notionalMinuteSQL = `(notional_trade_count > 0)`

// usdQuotePref ranks the USD quote forms (true USD, classic USDC, its SAC) when two tie on a
// bucket; without it the newest-bucket pick falls to scan order.
const usdQuotePref = `array_position(ARRAY['fiat:USD',
	'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
	'CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75'], quote_asset)`

// xlmFormPrefOpenBound2 ranks XLM's two on-chain forms the same way, classic first, with the SAC
// bound at $2; close it with the XLM-form column. Bound3/Bound4 bind $3/$4. Constants, not a
// helper, so the SQL vars that splice them stay compile-time constants.
const (
	xlmFormPrefOpenBound2 = `array_position(ARRAY['native', $2::text], `
	xlmFormPrefOpenBound3 = `array_position(ARRAY['native', $3::text], `
	xlmFormPrefOpenBound4 = `array_position(ARRAY['native', $4::text], `
)

// refreshAssetPriceSnapshotUpsert recomputes each priceable asset's price, 1h/24h/7d change and source count from [assetPriceCTEs].
// Money stays NUMERIC and unrounded (ADR-0003); the decimals correction happens here, before any rounding; computed_at is
// [rollupStampSQL].
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
		  -- per-asset arm, so it is seeded. It falls back out below when
		  -- xlm_usd is empty.
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
		  FROM priced_assets ca` + priceArmJoins(xlmUSDGridJoin) + `
		  -- Confirmed non-7-decimals tokens only; see
		  -- snapshotNormalizedPriceUSDExpr. The table is tiny (near-zero
		  -- confirmed offenders) and keyed on its primary key.
		  LEFT JOIN nonstandard_decimals_assets nda ON nda.asset = ca.asset_id
		)
		SELECT asset_id, price_usd, change_1h_pct, change_24h_pct,
		       change_7d_pct, source_count, ` + rollupStampSQL + `
		  FROM derived
		 WHERE price_usd IS NOT NULL
ON CONFLICT (asset_id) DO UPDATE
   SET price_usd      = EXCLUDED.price_usd,
       change_1h_pct  = EXCLUDED.change_1h_pct,
       change_24h_pct = EXCLUDED.change_24h_pct,
       change_7d_pct  = EXCLUDED.change_7d_pct,
       source_count   = EXCLUDED.source_count,
       computed_at    = EXCLUDED.computed_at`

// rollupStampSQL is both listing rollups' computed_at: the wall clock when the row is written, not the tx start, so a slow but successful
// sweep (two statements of up to 10 minutes) does not commit rows already past [assetPriceSnapshotMaxAge].
const rollupStampSQL = `clock_timestamp()`

// refreshAssetPriceSnapshotPrune drops assets no longer priceable this pass; just-upserted rows carry a stamp at or after the tx start
// (now()) and survive.
const refreshAssetPriceSnapshotPrune = `DELETE FROM asset_price_snapshot WHERE computed_at < now()`

// refreshAssetPriceSnapshotPruneExpired is the zero-row pass's prune ([pruneRollup]): past assetPriceSnapshotMaxAge the join ignores a row.
const refreshAssetPriceSnapshotPruneExpired = `DELETE FROM asset_price_snapshot WHERE computed_at < now() - INTERVAL '` + assetPriceSnapshotMaxAge + `'`

// pruneRollup drops rows this pass did not re-write; a zero-row upsert is an upstream fault (prices_1m empty or stalled), so it runs
// pruneExpired, which drops only rows past the validity bound.
func pruneRollup(ctx context.Context, tx *sql.Tx, upserted int64, prune, pruneExpired string) error {
	q := prune
	if upserted == 0 {
		q = pruneExpired
	}
	_, err := tx.ExecContext(ctx, q)
	return err
}

// execRowCount runs an upsert and returns how many rows it wrote.
func execRowCount(ctx context.Context, tx *sql.Tx, q string, args ...any) (int64, error) {
	res, err := tx.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// RefreshAssetListingRollups recomputes and atomically replaces both listing rollups (asset_volume_24h, asset_price_snapshot) in one
// transaction so a row's volume and price share a pass; upsert and prune take row locks only.
func (s *Store) RefreshAssetListingRollups(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("timescale: RefreshAssetListingRollups begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// This shares the primary with the customer-facing API, so bound it. work_mem: the 7-day
	// asset_vs_xlm DISTINCT ON sort spills at the 32 MB default; 96 MB removes the spill (64 MB does
	// not), per sort node. statement_timeout is a wedge guard, not a budget: on abort both rollups
	// keep their last good contents and the next tick retries. SET LOCAL reverts on COMMIT/ROLLBACK,
	// so nothing leaks onto the pooled connection.
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
	priceN, err := execRowCount(ctx, tx, refreshAssetPriceSnapshotUpsert, canonical.NativeSACContractID())
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
