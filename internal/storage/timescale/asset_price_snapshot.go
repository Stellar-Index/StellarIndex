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

// asset_price_snapshot: the per-asset headline-price rollup behind the
// GET /v1/assets listing.
//
// Deriving the price column per request meant twelve `DISTINCT ON … FROM
// prices_1m` CTEs for every asset on every uncached variant (2.4 s average,
// 10 s max, 51 MB disk sort on r1). So [assetPriceCTEs] runs here, off the
// request path, on the volume rollup's cadence, and the listing LEFT JOINs
// the result (the pattern of migrations 0087 and 0149).
//
// Not a continuous aggregate: the substrate is a `DISTINCT ON` over a UNION
// of quote forms, XLM identities and both directions, which no `GROUP BY`
// expresses. Not a materialised view: REFRESH takes ACCESS EXCLUSIVE and
// would stall the listing; the upsert+prune here takes row locks only, as
// [Store.RefreshAssetVolume24h] does.
//
// Rollup age (~5.5 min worst case) is capped by [assetPriceSnapshotMaxAge]
// in the listing's join, so a dead aggregator renders assets unpriced rather
// than serving old prices. Observation age is not capped: the price is the
// newest traded minute across arms ([priceArmPickExpr]) and can be up to the
// 7-day lookback old, since a hard cutoff would blank every weekly-trading
// asset.
//
// GET /v1/assets/{id} stays on its own single-asset query, so a listing row
// can lag its detail page by up to the ceiling above.

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
// itself, from the xlm_usd anchor), 'direct' (a USD-proxy market) or
// 'xlm' (an XLM market triangulated through the anchor `ax` taken at that
// market's own minute); NULL when none can.
//
// The arm with the NEWER observation wins. Choosing by arm order made
// any USD print in the 7-day lookback beat an XLM market trading this
// minute, so a days-old price was served, and fed market cap, as current.
// A triangulated observation is as old as its XLM-market minute, since the
// anchor is read as of that minute; on a tie the direct arm wins, as it has
// no triangulation error. An XLM market with no anchor within
// [xlmUSDAnchorMaxAge] of it cannot price, so an older direct row still
// does. Native XLM with no fresh anchor is unpriced rather than read from
// its own possibly week-old direct row: no fresh XLM/USD on any venue is an
// outage, and every XLM-quoted asset is unpriced with it.
const priceArmPickExpr = `CASE
		      WHEN ca.asset_id = 'native'
		        THEN CASE WHEN (SELECT vwap FROM xlm_usd) IS NOT NULL THEN 'native' END
		      WHEN direct.vwap IS NOT NULL
		           AND (ax.vwap IS NULL OR direct.bucket >= vs_xlm.bucket)
		        THEN 'direct'
		      WHEN ax.vwap IS NOT NULL
		        THEN 'xlm'
		    END`

// priceArmJoins attaches every arm CTE of [assetPriceArmCTEs] to the spine
// row `ca`, the XLM/USD anchor at each asset_vs_xlm* arm's own minute as
// `ax*` (anchorJoin renders one: the rollup joins the grid, the detail query
// a lateral pick), then picks the arm as `pick.arm`. Shared by the rollup
// and the detail query so the two cannot choose differently.
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

// snapshotPriceUSDExpr is the headline USD price, read through the arm
// [priceArmPickExpr] chose. The listing's `listingPriceUSDExpr` reads it
// back as `aps.price_usd`; the detail query renders it inline.
const snapshotPriceUSDExpr = `CASE pick.arm
		      WHEN 'native' THEN (SELECT vwap FROM xlm_usd)
		      WHEN 'direct' THEN direct.vwap
		      WHEN 'xlm'    THEN vs_xlm.vwap * ax.vwap
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
		      WHEN 'xlm' THEN CASE WHEN vs_xlm_%[1]s.vwap > 0 AND ax_%[1]s.vwap > 0
		        THEN ((vs_xlm.vwap * ax.vwap)
		            / (vs_xlm_%[1]s.vwap * ax_%[1]s.vwap) - 1) * 100 END
		    END`, lookback)
}

// snapshotNormalizedPriceUSDExpr is [snapshotPriceUSDExpr] with the
// dex-nonstandard-decimals forward normalisation applied; it is the value the
// rollup STORES. `nda` is the refresh's LEFT JOIN onto
// nonstandard_decimals_assets (migration 0093).
//
// prices_1m holds raw smallest-unit ratios, so for a token whose decimals()
// is not 7 every arm is off by the same 10^(7 - decimals); one factor
// corrects whichever arm answered, as v1.Server.normalizeCatalogueUSD does.
// It normalises at write because the table is overwritten each pass, every
// listing reader and the RWA market cap read this one column, and correcting
// before the listing's ROUND(price_usd, 10) keeps an 18-decimals token's
// 1e-11 raw ratio from rounding to zero.
//
// A READER OF THIS COLUMN MUST NOT NORMALISE IT AGAIN. The change columns
// need no factor: the scale cancels in each ratio.
//
// A MULTIPLIER OF THIS COLUMN MUST USE THE SAME DECIMALS: a market cap
// divides a smallest-unit supply by the token's real decimals, not 7
// (v1.Server.applyConfirmedListingDecimals,
// v1.Server.contractPriceScaleDisagrees).
//
// The CASE, rather than a COALESCE'd factor of 1, keeps unconfirmed assets'
// stored value byte-identical; power(numeric, numeric) with an integral
// exponent is exact (ADR-0003).
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

// unionPriceArmCTE renders the CTE pair `<name>_rows` / `<name>` for one
// price arm. Per asset, `<name>` carries the newest 1-minute bucket in window
// in which the asset traded against any of quotes, in EITHER stored
// direction; the VWAP of the union of that bucket's rows; and the distinct
// venues behind them.
//
// prices_1m keeps a market in whichever direction its source wrote it
// (Soroban AMMs: base = token_in; SDEX: the inverse) and `vwap` is always
// base priced in quote. So each row is re-expressed as two legs in the arm's
// (asset, quote) orientation and the leg sums re-divided, the SQL form of
// [combineDirVWAP]:
//
//	(asset, q) row: asset leg = volume_priced,         q leg = vwap × volume_priced
//	(q, asset) row: asset leg = vwap × volume_priced,  q leg = volume_priced
//
// volume_priced, not volume: vwap covers only trades with both legs > 0
// (migration 0187), so its weight must too. Preferring one direction prices
// an asset from a side of its book that may be days stale.
//
// asset is "" for every asset (the rollup) or a scalar SQL expression that
// pins the arm to one asset (the detail query).
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
		          SELECT base_asset AS asset_id, bucket, volume_priced AS asset_leg,
		                 vwap * volume_priced AS quote_leg, sources
		            FROM prices_1m
		           WHERE quote_asset IN (%[2]s)%[4]s
		             AND %[3]s
		             AND vwap > 0 AND volume_priced > 0
		          UNION ALL
		          SELECT quote_asset, bucket, vwap * volume_priced, volume_priced, sources
		            FROM prices_1m
		           WHERE base_asset IN (%[2]s)%[5]s
		             AND %[3]s
		             AND vwap > 0 AND volume_priced > 0
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
// pins both IN-lists. xlmQuoteList is an [xlmQuotesBound] form; the
// caller binds the network's native SAC at that placeholder.
func assetPriceArmCTEs(asset, xlmQuoteList string) string {
	arms := make([]string, 0, 8)
	for _, arm := range []struct{ name, quotes string }{
		{"direct_usd", usdProxyQuotes},
		{"asset_vs_xlm", xlmQuoteList},
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
// arms, XLM's own USD scalars, and the anchor grid the asset_vs_xlm* arms
// are triangulated through, reaching back to the oldest arm's window. The
// `/*PUSHDOWN_*/` markers the listing once carried are gone: a full
// all-asset recompute has nothing to narrow to (same reason
// refreshAssetVolumeUpsert carries none). $1 is
// canonical.NativeSACContractID().
var assetPriceCTEs = assetPriceArmCTEs("", xlmQuotesBound1) + "," +
	xlmUSDNativeCTEs(nativeSACParam(1)) + "," +
	xlmUSDAnchorGridCTE("xlm_usd_grid", priceWindow7dLo, nativeSACParam(1))

// usdQuotePref ranks the USD quote forms for a pick that two forms can tie
// on the same bucket: a true USD quote, then classic USDC, then its SAC
// (thinnest last). Without it the newest-bucket pick falls to scan order.
const usdQuotePref = `array_position(ARRAY['fiat:USD',
	'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
	'CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75'], quote_asset)`

// xlmFormPrefOpenBound2 ranks XLM's two on-chain forms the same way, classic
// first, with the SAC bound at $2; close it with the column holding the XLM
// form. Bound3/Bound4 bind $3/$4. Constants (not a helper) so the SQL vars
// that splice them stay compile-time constants.
const (
	xlmFormPrefOpenBound2 = `array_position(ARRAY['native', $2::text], `
	xlmFormPrefOpenBound3 = `array_position(ARRAY['native', $3::text], `
	xlmFormPrefOpenBound4 = `array_position(ARRAY['native', $4::text], `
)

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
func execRowCount(ctx context.Context, tx *sql.Tx, q string, args ...any) (int64, error) {
	res, err := tx.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// RefreshAssetListingRollups recomputes BOTH rollups the /v1/assets
// listing LEFT JOINs — asset_volume_24h (migration 0087) and
// asset_price_snapshot (migration 0154) — and atomically
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
	// does not fit r1's 32 MB session default: measured on r1 it
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
