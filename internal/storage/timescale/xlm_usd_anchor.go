// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

// The XLM/USD anchor: the one rate every read-side XLM-triangulated price
// is multiplied by. Every renderer below reads the same population through
// [xlmUSDAnchorMinutes], so the scalar, grid and per-period shapes cannot
// disagree about which rows count.
//
// Population: XLM in every alias form ([canonical.AssetAliases] order:
// native, crypto:XLM, the SAC) against every USD proxy ([usdProxyQuotes]),
// in BOTH stored directions — SDEX stores the offer's sold asset as base and
// swap-direction venues store token_in, so (USDC, native) and
// (USDC-SAC, XLM-SAC) rows carry real XLM/USD trades; they enter as 1/vwap.
// Closed minutes only (ADR-0015).
//
// Per minute and form the rate is the volume_usd-weighted mean of those rows.
// volume_usd, never the leg sums: CEX amounts are scaled 1e8 and on-chain
// ones 1e7, so leg sums would weight a CEX row 10x; volume_usd is already USD
// at each source's own scale.
//
// Across forms the FIRST form (alias order) with a qualifying minute in the
// freshness window wins, newest minute within it — the same contract as the
// ingest resolver's alias loop ([VWAPUSDFXResolver.queryDB]): SAC last, so a
// thin Soroban pool printing after a quiet SDEX minute cannot set the rate
// for the whole catalogue. Within one form a lone dust row is still the
// minute's rate when nothing else printed; the per-row floor matches the
// resolver's [directLegMinQuoteVolume].

// xlmUSDAnchorMaxAge is how old the XLM/USD minute behind a triangulated price
// may be at the instant it is applied: the ingest resolver's default
// Freshness, so read time and trade time agree on when XLM has a USD price.
// Past it the asset is unpriced, never re-marked at a stale rate.
const xlmUSDAnchorMaxAge = "1 hour"

// xlmUSDAnchorMinUSD is the per-row USD floor; volume_usd is
// sum(coalesce(usd_volume, 0)), so it also drops rows nothing stamped.
const xlmUSDAnchorMinUSD = "0.01"

// xlmUSDAnchorForms is XLM's alias forms in priority order with the SAC as
// sac (the pubnet literal for the writer, a bound `$N::text` for reads).
func xlmUSDAnchorForms(sac string) string {
	return "'native', 'crypto:XLM', " + sac
}

// xlmUSDAnchorMinutes renders a subquery yielding (form_rank, bucket, vwap):
// XLM's USD rate per minute and form for closed minutes in [lo, hi]. lo and
// hi are SQL timestamptz expressions; keep them sargable (no function of
// bucket).
func xlmUSDAnchorMinutes(lo, hi, sac string) string {
	forms := xlmUSDAnchorForms(sac)
	bounds := `bucket >= ` + lo + `
		         AND bucket <= ` + hi + `
		         AND bucket <= now() - INTERVAL '1 minute'
		         AND vwap > 0 AND volume_usd >= ` + xlmUSDAnchorMinUSD
	return `(SELECT form_rank, bucket, sum(px * volume_usd) / sum(volume_usd) AS vwap
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

// xlmUSDAnchorAt renders [xlmUSDAnchorPick] as of instant ts: no row when no
// form printed within [xlmUSDAnchorMaxAge] before it. For single-row paths;
// batch paths join [xlmUSDAnchorGridCTE], which TestXLMUSDAnchor_ShapesAgree
// holds to the same value.
func xlmUSDAnchorAt(ts, sac string) string {
	return xlmUSDAnchorPick("("+ts+") - INTERVAL '"+xlmUSDAnchorMaxAge+"'", ts, sac)
}

// xlmUSDAnchorGridCTE renders CTEs `<name>_min` and `<name>`(minute, vwap,
// bucket): [xlmUSDAnchorAt] evaluated at every minute from lo to now, for
// equi-joining on a 1-minute bucket. One range scan plus a forward fill per
// form (the count-group trick), instead of an index probe per joined row.
// The grid starts [xlmUSDAnchorMaxAge] before lo so the fill is seeded at lo.
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

// xlmUSDAnchorPerPeriodCTE renders CTE `name`(col, vwap): the anchor per
// date_trunc(unit) period since lo — highest-priority form that printed in
// the period, its newest minute. The sparklines' XLM leg.
func xlmUSDAnchorPerPeriodCTE(name, col, unit, lo, sac string) string {
	return `
		` + name + ` AS (
		  SELECT DISTINCT ON (1) date_trunc('` + unit + `', xm.bucket) AS ` + col + `, xm.vwap
		    FROM ` + xlmUSDAnchorMinutes(lo, "now()", sac) + ` xm
		   ORDER BY 1, xm.form_rank, xm.bucket DESC
		)`
}

// xlmUSDNativeCTEs renders XLM's own USD price now (`xlm_usd`) and at each
// change lookback (`xlm_usd_1h/_24h/_7d`, read within the same tolerance
// windows as the per-asset arms), each one (vwap, bucket) row or none.
func xlmUSDNativeCTEs(sac string) string {
	return `
		xlm_usd AS (` + xlmUSDAnchorAt("now()", sac) + `),
		xlm_usd_1h AS (` + xlmUSDAnchorPick(priceWindow1hLo, priceWindow1hHi, sac) + `),
		xlm_usd_24h AS (` + xlmUSDAnchorPick(priceWindow24hLo, priceWindow24hHi, sac) + `),
		xlm_usd_7d AS (` + xlmUSDAnchorPick(priceWindow7dLo, priceWindow7dHi, sac) + `)`
}
