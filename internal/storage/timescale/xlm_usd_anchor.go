// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

// The XLM/USD anchor: the one rate every read-side XLM-triangulated price is multiplied by. Every renderer
// reads the same population through [xlmUSDAnchorMinutes], so the scalar, grid and per-period shapes agree.
//
// Population: XLM in every alias form ([canonical.AssetAliases] order: native, crypto:XLM, the SAC)
// against every USD proxy ([usdProxyQuotes]), in both stored directions (SDEX stores the sold asset
// as base, swap venues store token_in), so reversed rows enter as 1/vwap. Closed minutes only, none
// inside a recorded XLM/USD-proxy freeze on any XLM form (ADR-0019, freeze_events frozen_at to
// recovered_at): a frozen rate anchors nothing, the newest unfrozen minute does, and a freeze longer
// than [xlmUSDAnchorMaxAge] leaves every consumer (catalogue price and changes, sparklines,
// transitive prices, Soroban volume, MEV legs) unpriced, as the max age already does for silence.
// An open row (recovered_at NULL) excludes every later minute until [FreezeEventSink.MarkRecovered]
// closes it; hold_until is the ladder deadline, not the freeze's end, and does not bound the predicate.
//
// Per minute and form the rate is the volume_usd-weighted mean of those rows, never the leg sums:
// CEX amounts are scaled 1e8 and on-chain ones 1e7, so leg sums would weight a CEX row 10x.
//
// Across forms the first form (alias order) with a qualifying minute in the freshness window wins, newest
// minute within it, matching the ingest resolver's alias loop ([VWAPUSDFXResolver.queryDB]). SAC is last so a
// thin Soroban pool printing after a quiet SDEX minute cannot set the rate for the whole catalogue. Within one
// form a lone dust row is still the minute's rate when nothing else printed (per-row floor [directLegMinQuoteVolume]).

// xlmUSDAnchorMaxAge is the oldest XLM/USD minute a triangulated price may use, the ingest resolver's Freshness; past it the asset is
// unpriced, never re-marked.
const xlmUSDAnchorMaxAge = "1 hour"

// xlmUSDAnchorMinUSD is the per-row USD floor; volume_usd is
// sum(coalesce(usd_volume, 0)), so it also drops rows nothing stamped.
const xlmUSDAnchorMinUSD = "0.01"

// xlmUSDAnchorForms is XLM's alias forms in priority order with the SAC as
// sac (the pubnet literal for the writer, a bound `$N::text` for reads).
func xlmUSDAnchorForms(sac string) string {
	return "'native', 'crypto:XLM', " + sac
}

// xlmUSDFreezeTriggerLookback extends a freeze back before frozen_at, the detector's wall clock, written a tick or more after the
// anomalous minutes closed; the detector's hot window (orchestrator.DefaultWindows) is how far a steady-state trigger trails them.
// A warm or cold-window trigger, or a catch-up after a stall, trails further: those minutes still anchor, flagged estimated.
const xlmUSDFreezeTriggerLookback = "5 minutes"

// xlmUSDFreezeOverlap renders "a recorded XLM/USD-proxy freeze (ADR-0019) overlaps [from, bucket + 1 minute +
// xlmUSDFreezeTriggerLookback)": any XLM form, either stored direction, open or recovered. The anchor population probes the minute
// itself (from = bucket); a read that judges a stored stamp looks back xlmUSDAnchorMaxAge from it, the oldest minute the ingest
// resolver may have read when it stamped the row.
func xlmUSDFreezeOverlap(from, bucket, sac string) string {
	return `EXISTS (SELECT 1 FROM freeze_events fe
		                  WHERE ` + xlmUSDFreezePairs(sac) + `
		                    AND ` + xlmUSDFreezeWindow("fe", from, bucket) + `)`
}

// xlmUSDFreezePairs renders "fe is a freeze on an XLM form against a USD proxy", either stored direction.
func xlmUSDFreezePairs(sac string) string {
	forms := xlmUSDAnchorForms(sac)
	return `((fe.asset_id IN (` + forms + `) AND fe.quote_id IN (` + usdProxyQuotes + `))
		                     OR (fe.quote_id IN (` + forms + `) AND fe.asset_id IN (` + usdProxyQuotes + `)))`
}

// xlmUSDFreezeWindow renders "freeze row rel overlaps [from, bucket + 1 minute + xlmUSDFreezeTriggerLookback)".
func xlmUSDFreezeWindow(rel, from, bucket string) string {
	return rel + `.frozen_at < ` + bucket + ` + INTERVAL '1 minute' + INTERVAL '` + xlmUSDFreezeTriggerLookback + `'
		                    AND (` + rel + `.recovered_at IS NULL OR ` + rel + `.recovered_at > ` + from + `)`
}

// xlmUSDAnchorMinutes renders (form_rank, bucket, vwap) for closed minutes in [lo, hi]; lo and hi are timestamptz expressions that must
// stay sargable. The XLM/USD freezes are read once into fz: probing freeze_events per minute row was most of a 7d sparkline's cost.
func xlmUSDAnchorMinutes(lo, hi, sac string) string {
	forms := xlmUSDAnchorForms(sac)
	bounds := `bucket >= ` + lo + `
		         AND bucket <= ` + hi + `
		         AND bucket <= now() - INTERVAL '1 minute'
		         AND vwap > 0 AND volume_usd >= ` + xlmUSDAnchorMinUSD + `
		         AND NOT EXISTS (SELECT 1 FROM fz WHERE ` + xlmUSDFreezeWindow("fz", "prices_1m.bucket", "prices_1m.bucket") + `)`
	return `(WITH fz AS MATERIALIZED (SELECT fe.frozen_at, fe.recovered_at FROM freeze_events fe WHERE ` + xlmUSDFreezePairs(sac) + `)
		  SELECT form_rank, bucket, sum(px * volume_usd) / sum(volume_usd) AS vwap
		    FROM (
		      SELECT array_position(ARRAY[` + forms + `], base_asset) AS form_rank,
		             bucket, vwap AS px, volume_usd
		        FROM prices_1m
		       WHERE base_asset IN (` + forms + `)
		         AND quote_asset IN (` + usdProxyQuotes + `)
		         AND ` + bounds + `
		      UNION ALL
		      SELECT array_position(ARRAY[` + forms + `], quote_asset),
		             bucket, 1 / vwap, volume_usd
		        FROM prices_1m
		       WHERE quote_asset IN (` + forms + `)
		         AND base_asset IN (` + usdProxyQuotes + `)
		         AND ` + bounds + `
		    ) xr
		   GROUP BY form_rank, bucket)`
}

// xlmUSDAnchorPick renders a query returning at most one (vwap, bucket) row:
// the anchor over minutes in [lo, hi] — highest-priority form, newest minute.
func xlmUSDAnchorPick(lo, hi, sac string) string {
	return `SELECT xm.vwap, xm.bucket
		    FROM ` + xlmUSDAnchorMinutes(lo, hi, sac) + ` xm
		   ORDER BY xm.form_rank, xm.bucket DESC
		   LIMIT 1`
}

// xlmUSDAnchorAt renders [xlmUSDAnchorPick] as of ts: no row if no form printed within [xlmUSDAnchorMaxAge]. Batch paths join
// [xlmUSDAnchorGridCTE]; TestXLMUSDAnchor_ShapesShareOnePopulation holds them equal.
func xlmUSDAnchorAt(ts, sac string) string {
	return xlmUSDAnchorPick("("+ts+") - INTERVAL '"+xlmUSDAnchorMaxAge+"'", ts, sac)
}

// xlmUSDAnchorGridCTE renders CTEs `<name>_min` and `<name>`(minute, vwap, bucket): [xlmUSDAnchorAt] at every minute from lo to now. One
// range scan plus a per-form forward fill replaces a probe per row; the grid starts [xlmUSDAnchorMaxAge] before lo to seed the fill.
func xlmUSDAnchorGridCTE(name, lo, sac string) string {
	start := "(" + lo + ") - INTERVAL '" + xlmUSDAnchorMaxAge + "'"
	return `
		` + name + `_min AS (
		  SELECT form_rank, bucket, vwap FROM ` + xlmUSDAnchorMinutes(start, "now()", sac) + ` xm
		),
		` + name + ` AS (
		  SELECT DISTINCT ON (minute) minute, vwap, bucket
		    FROM (
		      SELECT minute, form_rank,
		             first_value(vwap)   OVER fill AS vwap,
		             first_value(bucket) OVER fill AS bucket
		        FROM (
		          SELECT g.minute, f.form_rank, m.vwap, m.bucket,
		                 count(m.bucket) OVER (PARTITION BY f.form_rank ORDER BY g.minute) AS run
		            FROM generate_series(date_trunc('minute', ` + start + `), now(), INTERVAL '1 minute') AS g(minute)
		           CROSS JOIN (VALUES (1), (2), (3)) AS f(form_rank)
		            LEFT JOIN ` + name + `_min m ON m.form_rank = f.form_rank AND m.bucket = g.minute
		        ) s
		      WINDOW fill AS (PARTITION BY form_rank, run ORDER BY minute)
		    ) filled
		   WHERE bucket >= minute - INTERVAL '` + xlmUSDAnchorMaxAge + `'
		   ORDER BY minute, form_rank
		)`
}

// xlmUSDAnchorPerPeriodCTE renders CTE `name`(col, vwap): the anchor per date_trunc(unit) period
// since lo, from the highest-priority form that printed in it (newest minute). The sparklines' XLM leg.
func xlmUSDAnchorPerPeriodCTE(name, col, unit, lo, sac string) string {
	return `
		` + name + ` AS (
		  SELECT DISTINCT ON (1) date_trunc('` + unit + `', xm.bucket) AS ` + col + `, xm.vwap
		    FROM ` + xlmUSDAnchorMinutes(lo, "now()", sac) + ` xm
		   ORDER BY 1, xm.form_rank, xm.bucket DESC
		)`
}

// xlmUSDNativeCTEs renders XLM's USD price now (`xlm_usd`) and at each change lookback (`xlm_usd_1h/_24h/_7d`), each one (vwap, bucket)
// row or none.
func xlmUSDNativeCTEs(sac string) string {
	return `
		xlm_usd AS (` + xlmUSDAnchorAt("now()", sac) + `),
		xlm_usd_1h AS (` + xlmUSDAnchorPick(priceWindow1hLo, priceWindow1hHi, sac) + `),
		xlm_usd_24h AS (` + xlmUSDAnchorPick(priceWindow24hLo, priceWindow24hHi, sac) + `),
		xlm_usd_7d AS (` + xlmUSDAnchorPick(priceWindow7dLo, priceWindow7dHi, sac) + `)`
}
