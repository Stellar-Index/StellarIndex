package timescale

// DEX/AMM bespoke analytics (soroswap / phoenix / aquarius / comet / sdex). Three data tiers,
// chosen per query so nothing scans the 300M+-row trades hypertable unbounded:
//   1. dex_volume_by_pair_1d (materialized_only=true) for >1-day windows. HONESTY FLOOR: it starts
//      well after genesis, so lifetime figures are "since <floor date>", never "all-time".
//   2. source_volume_1h (real-time) for the 24h window.
//   3. Raw `trades`, the only place taker addresses live, window-bounded; sdex is gated OFF beyond
//      24h (dexRawWindowOK) and the omission is Noted.
//
// Older soroswap rows carry NULL taker, so trader metrics are omitted (with a Note) unless the
// window has taker-stamped rows. USD figures are trade-time trades.usd_volume (or its CAGG sums),
// never ad-hoc pricing; at 24h unpriced XLM legs are named and the figure is a lower bound,
// elsewhere NULL-usd trades are excluded from USD sums but still count toward totals. Division is
// exact NUMERIC (ADR-0003); rounding is round(x, 2), never a float.

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/currency"
)

// Window-stable series names. The explorer renders standalone series one
// panel each and folds "<Group> · <entity>" series into one multi-line
// panel per group, so the top-pair lines share the dexTopPairPrefix.
const (
	dexSeriesVolume  = "USD volume"
	dexSeriesTrades  = "Trades"
	dexSeriesTraders = "Unique traders"
	dexTopPairPrefix = "Top pairs · "
)

// dexRawWindowOK reports whether raw-trades surfaces are servable for source at this window; sdex is too big (10.7M rows/7d, 45s on
// r1, past the one-minute build budget), so 24h only.
func dexRawWindowOK(source string, windowDays int) bool {
	return source != "sdex" || windowDays <= 1
}

// Query builders: every windowed query binds source=$1 and window=$2::interval and bounds its time
// column through dexWindowSQL; the since-totals query binds source=$1 only.

// dexWindowSQL is the window predicate on col: 24h rolls; longer windows are the N complete UTC days before today, since the daily CAGG
// never materializes today and a rolling cut would sum N-1 days. Raw-trade figures share the bounds.
func dexWindowSQL(windowDays int, col string) string {
	if windowDays == 1 {
		return col + " > now() - $2::interval"
	}
	return col + " >= " + completeDayCutoffSQL + " - $2::interval AND " + col + " < " + completeDayCutoffSQL
}

// dexPricedUSD is the source_volume_1h read contract: sum only sum_usd_priced (trade-time usd_volume); valuing XLM legs at spot would make
// history move. The unpriced XLM is reported via dexXLMLegUnvalued so the figure is a named lower bound.
const dexPricedUSD = "COALESCE(sum(sum_usd_priced),0)"

// dexXLMLegUnvalued is the unpriced XLM excluded from dexPricedUSD (empty when none); the divisor is 10000000 as assertDEXNumericSafe
// rejects exponents.
const dexXLMLegUnvalued = `COALESCE(CASE WHEN COALESCE(sum(sum_xlm_base),0) + COALESCE(sum(sum_xlm_quote),0) > 0
		         THEN round((COALESCE(sum(sum_xlm_base),0) + COALESCE(sum(sum_xlm_quote),0))
		           / 10000000::numeric, 7)::text END, '')`

// dexHourlyBucketWindow is the shared trailing-interval predicate for source_volume_1h readers: strict `>` against now(), never `>=` or a
// date_trunc floor, which would pull in a 25th bucket and disagree with dexWindowKPIQuery/dexActivitySeriesQuery.
func dexHourlyBucketWindow(intervalParam int) string {
	return fmt.Sprintf("bucket > now() - $%d::interval", intervalParam)
}

// dexActivitySeriesQuery returns (bucket, usd_volume, trades) at the window's grain: hourly from source_volume_1h at 24h, else
// dex_volume_by_pair_1d.
func dexActivitySeriesQuery(windowDays int) string {
	if windowDays == 1 {
		return `
			SELECT to_char(bucket, 'YYYY-MM-DD"T"HH24:00'), (` + dexPricedUSD + `)::text, COALESCE(sum(trade_count),0)::text
			FROM source_volume_1h WHERE source = $1 AND ` + dexWindowSQL(windowDays, "bucket") + `
			GROUP BY 1 ORDER BY 1 ASC`
	}
	return `
		SELECT to_char(bucket, 'YYYY-MM-DD'), COALESCE(sum(vol),0)::text, COALESCE(sum(trades),0)::text
		FROM dex_volume_by_pair_1d WHERE source = $1 AND ` + dexWindowSQL(windowDays, "bucket") + `
		GROUP BY 1 ORDER BY 1 ASC`
}

// dexWindowKPIQuery returns (usd_volume, trades, active_pairs). The >1d form counts pairs via a hash-agg subquery, not count(DISTINCT
// (base,quote)) (~9x slower at 90d sdex); the 24h form has no pair dimension, so the caller fills pairs from the raw KPI query.
func dexWindowKPIQuery(windowDays int) string {
	if windowDays == 1 {
		return `
			SELECT round(` + dexPricedUSD + `,2)::text, COALESCE(sum(trade_count),0)::text,
			       ` + dexXLMLegUnvalued + `
			FROM source_volume_1h WHERE source = $1 AND ` + dexWindowSQL(windowDays, "bucket")
	}
	return `
		SELECT round(COALESCE(sum(vol),0),2)::text, COALESCE(sum(trades),0)::text,
		       (SELECT count(*) FROM (
		          SELECT 1 FROM dex_volume_by_pair_1d
		           WHERE source = $1 AND ` + dexWindowSQL(windowDays, "bucket") + `
		           GROUP BY ` + marketKeySQL + `) q)::text
		FROM dex_volume_by_pair_1d WHERE source = $1 AND ` + dexWindowSQL(windowDays, "bucket")
}

// dexRawKPIQuery returns (unique_takers, priced_trades, active_pairs, avg_trade_usd) from raw trades; the average is exact NUMERIC division
// by the PRICED trade count (ADR-0003), rounded to cents. sdex is gated by dexRawWindowOK.
func dexRawKPIQuery(windowDays int) string {
	return `
		SELECT count(DISTINCT taker)::text, count(usd_volume)::text,
		       count(DISTINCT (` + marketKeySQL + `))::text,
		       COALESCE(round(sum(usd_volume) / NULLIF(count(usd_volume),0), 2), 0)::text
		FROM trades WHERE source = $1 AND ` + dexWindowSQL(windowDays, "ts")
}

// dexTradersSeriesQuery returns the per-bucket unique-taker series from raw trades (bridgeSeriesGrain); no CAGG carries takers.
func dexTradersSeriesQuery(windowDays int) string {
	trunc, format := bridgeSeriesGrain(windowDays)
	return `
		SELECT to_char(date_trunc('` + trunc + `', ts), '` + format + `'), count(DISTINCT taker)::text
		FROM trades WHERE source = $1 AND ` + dexWindowSQL(windowDays, "ts") + ` AND taker IS NOT NULL
		GROUP BY 1 ORDER BY 1 ASC`
}

// dexPairBreakdownQuery returns the top 8 pairs by USD volume plus a fold-the-rest row with empty pair columns; ranking happens in SQL so
// sdex's ~99k pairs stay off the wire. The 24h form reads raw trades because the daily CAGG cannot bucket 24h.
func dexPairBreakdownQuery(windowDays int) string {
	var p string
	if windowDays == 1 {
		p = `SELECT base_asset, quote_asset, COALESCE(sum(usd_volume),0) AS vol, count(*) AS n
		    FROM trades
		   WHERE source = $1 AND ` + dexWindowSQL(windowDays, "ts") + `
		   GROUP BY 1, 2`
	} else {
		p = `SELECT base_asset, quote_asset, COALESCE(sum(vol),0) AS vol, sum(trades) AS n
		    FROM dex_volume_by_pair_1d
		   WHERE source = $1 AND ` + dexWindowSQL(windowDays, "bucket") + `
		   GROUP BY 1, 2`
	}
	return `
		WITH p AS (` + p + `),
		 r AS (SELECT p.*, row_number() OVER (ORDER BY vol DESC, n DESC) AS rn FROM p)
		SELECT CASE WHEN rn <= 8 THEN base_asset ELSE '' END,
		       CASE WHEN rn <= 8 THEN quote_asset ELSE '' END,
		       round(sum(vol),2)::text, sum(n)
		FROM r GROUP BY 1, 2 ORDER BY min(rn) ASC`
}

// dexTopPairsSeriesQuery returns (base, quote, bucket, usd) for the top-5 pairs, ordered by window volume desc then bucket. The >1d form
// joins the daily CAGG directly (rows already unique); re-aggregating through a grouped CTE is ~20x slower at 90d sdex.
func dexTopPairsSeriesQuery(windowDays int) string {
	if windowDays == 1 {
		return `
			WITH p AS (
			  SELECT base_asset, quote_asset, date_trunc('hour', ts) AS bucket, COALESCE(sum(usd_volume),0) AS vol
			    FROM trades
			   WHERE source = $1 AND ` + dexWindowSQL(windowDays, "ts") + `
			   GROUP BY 1, 2, 3),
			 top AS (
			  SELECT base_asset, quote_asset, sum(vol) AS total
			    FROM p GROUP BY 1, 2
			   ORDER BY total DESC NULLS LAST LIMIT 5)
			SELECT p.base_asset, p.quote_asset, to_char(p.bucket, 'YYYY-MM-DD"T"HH24:00'), p.vol::text
			FROM p JOIN top USING (base_asset, quote_asset)
			ORDER BY top.total DESC NULLS LAST, p.base_asset, p.quote_asset, p.bucket ASC`
	}
	return `
		WITH top AS (
		  SELECT base_asset, quote_asset, sum(vol) AS total
		    FROM dex_volume_by_pair_1d
		   WHERE source = $1 AND ` + dexWindowSQL(windowDays, "bucket") + `
		   GROUP BY 1, 2 ORDER BY total DESC NULLS LAST LIMIT 5)
		SELECT t.base_asset, t.quote_asset, to_char(t.bucket, 'YYYY-MM-DD'), COALESCE(t.vol,0)::text
		FROM dex_volume_by_pair_1d t JOIN top USING (base_asset, quote_asset)
		WHERE t.source = $1 AND ` + dexWindowSQL(windowDays, "t.bucket") + `
		ORDER BY top.total DESC NULLS LAST, t.base_asset, t.quote_asset, t.bucket ASC`
}

// dexTopPairsTableQuery returns the top-25 pairs table rows; the 24h form reads raw trades for the same reason as the breakdown.
func dexTopPairsTableQuery(windowDays int) string {
	if windowDays == 1 {
		return `
			SELECT base_asset, quote_asset,
			       count(*)::text, round(COALESCE(sum(usd_volume),0),2)::text, COALESCE(sum(base_amount),0)::text
			  FROM trades
			 WHERE source = $1 AND ` + dexWindowSQL(windowDays, "ts") + `
			 GROUP BY base_asset, quote_asset
			 ORDER BY sum(usd_volume) DESC NULLS LAST, count(*) DESC LIMIT 25`
	}
	return `
		SELECT base_asset, quote_asset,
		       COALESCE(sum(trades),0)::text, round(COALESCE(sum(vol),0),2)::text, COALESCE(sum(base_vol),0)::text
		  FROM dex_volume_by_pair_1d
		 WHERE source = $1 AND ` + dexWindowSQL(windowDays, "bucket") + `
		 GROUP BY base_asset, quote_asset
		 ORDER BY sum(vol) DESC NULLS LAST, sum(trades) DESC LIMIT 25`
}

// dexLargestTradesQuery returns the top-10 trades by raw usd_volume from raw trades; amounts stay per-asset base-unit NUMERIC strings.
func dexLargestTradesQuery(windowDays int) string {
	return `
		SELECT base_asset, quote_asset, base_amount::text, quote_amount::text,
		       round(usd_volume,2)::text, tx_hash, to_char(ts, 'YYYY-MM-DD')
		FROM trades WHERE source = $1 AND ` + dexWindowSQL(windowDays, "ts") + ` AND usd_volume IS NOT NULL
		ORDER BY usd_volume DESC LIMIT 10`
}

// dexSinceTotalsQuery returns unwindowed totals over ALL materialized buckets; not "all-time" as the rollup floor is after genesis, so the
// floor date is served.
func dexSinceTotalsQuery() string {
	return `
		SELECT round(COALESCE(sum(vol),0),2)::text, COALESCE(sum(trades),0)::text, COALESCE(min(bucket)::date::text, '')
		FROM dex_volume_by_pair_1d WHERE source = $1`
}

// ─── pair labelling ──────────────────────────────────────────────────────

// dexSACSymbols lazily builds the token-contract to verified-ticker map for pair labels (native SAC to XLM, verified issuances), once per
// process.
var dexSACSymbols = sync.OnceValue(func() map[string]string {
	m := map[string]string{}
	if sac, err := canonical.NativeAsset().SacContractID(); err == nil {
		m[sac] = "XLM"
	}
	cat, err := currency.LoadEmbedded()
	if err != nil {
		return m
	}
	for _, vc := range cat.All() {
		e := vc.StellarEntry()
		if e == nil || e.AssetID == "" || e.AssetID == "native" || vc.Ticker == "" {
			continue
		}
		a, err := canonical.ParseAsset(e.AssetID)
		if err != nil {
			continue
		}
		switch a.Type {
		case canonical.AssetClassic:
			if sac, err := a.SacContractID(); err == nil {
				m[sac] = vc.Ticker
			}
		case canonical.AssetSoroban:
			m[a.ContractID] = vc.Ticker
		case canonical.AssetNative, canonical.AssetFiat, canonical.AssetCrypto,
			canonical.AssetRWA, canonical.AssetOracleRaw:
			// native handled above; fiat/crypto/RWA/raw-oracle ids never
			// appear as trade-leg token contracts.
		}
	}
	return m
})

// dexAssetLabel renders a trade-leg asset id as a short symbol: XLM, the classic CODE, the verified ticker, else a truncated contract;
// never guessed.
func dexAssetLabel(asset string) string {
	if asset == "native" {
		return "XLM"
	}
	if canonical.IsContractID(asset) {
		if sym, ok := dexSACSymbols()[asset]; ok {
			return sym
		}
		return asset[:4] + "…" + asset[len(asset)-4:]
	}
	if code, _, ok := strings.Cut(asset, "-"); ok && code != "" {
		return code
	}
	return asset
}

// dexPairLabel renders a base/quote pair ("XLM/USDC").
func dexPairLabel(base, quote string) string {
	return dexAssetLabel(base) + "/" + dexAssetLabel(quote)
}

// ─── block assembly ──────────────────────────────────────────────────────

// dexUSDValuationNote states how USD is derived (trade-time usd_volume only); at 24h xlmUnvalued names the unpriced XLM that makes the KPI
// a lower bound.
func dexUSDValuationNote(windowDays int, xlmUnvalued string) string {
	if windowDays == 1 && xlmUnvalued != "" {
		return "USD figures are sums of the ingest-time trades.usd_volume valuation. " + xlmUnvalued + " XLM of XLM-denominated legs that valuation left unpriced are NOT valued: the 24h volume KPI and hourly volume series are lower bounds covering priced trades only. Trades no path can price are excluded from USD sums and averages but still count toward trade totals."
	}
	return "USD figures are sums of the ingest-time trades.usd_volume valuation only — never ad-hoc pricing. Trades whose quote never resolved to a USD price are excluded from USD sums and averages but still count toward trade totals."
}

// dexCompleteDaysNote discloses the >1d window (see dexWindowSQL) and refresh lag: migration 0064 materializes a finished day only after
// 01:00 UTC.
func dexCompleteDaysNote(windowDays int) string {
	return fmt.Sprintf("Every (%dd) figure covers the %d complete UTC days before today; today's partial day is excluded. The daily rollup behind USD volume, trades, active pairs, the pair breakdown and the top-pairs chart and table adds a finished day within about two hours of 00:00 UTC, so until then those figures may not yet include yesterday.", windowDays, windowDays)
}

// bespokeDEX builds the DEX/AMM bespoke block. See the file doc for the
// three data tiers and the honesty rules each sub-part encodes.
func (s *Store) bespokeDEX(ctx context.Context, source string, windowDays int) (*BespokeBlock, error) {
	since := fmt.Sprintf("%d days", windowDays)
	blk := &BespokeBlock{
		Category: "dex",
		Notes: []string{
			dexUSDValuationNote(windowDays, ""),
			"Pairs are labelled by verified token tickers (native XLM plus the verified-currency catalogue's SAC/token addresses); an unverified token shows a truncated contract id, never a guessed symbol. Base/quote amounts are token base units at per-asset decimals, not USD.",
		},
	}

	if windowDays > 1 {
		blk.Notes = append(blk.Notes, dexCompleteDaysNote(windowDays))
	}

	xlmUnvalued, err := s.dexWindowBlocks(ctx, blk, source, since, windowDays)
	if err != nil {
		return nil, err
	}
	blk.Notes[0] = dexUSDValuationNote(windowDays, xlmUnvalued)
	if err := s.dexSinceTotalsKPIs(ctx, blk, source); err != nil {
		return nil, err
	}

	// Per-source captured-liquidity augments (reserves / net-flow depth /
	// staking / skim).
	if err := s.dexSourceAugments(ctx, blk, source, windowDays); err != nil {
		return nil, err
	}

	// Omit an all-empty block — a dormant DEX, or soroswap-router whose
	// swaps are ContractCall-derived and never land in `trades` — rather
	// than render an all-zero DEX panel.
	if len(blk.KPIs) == 0 {
		return nil, nil
	}
	return blk, nil
}

// dexWindowBlocks fills every window-scoped sub-part (nothing for zero trades) and returns the KPI's xlmUnvalued for the block note's
// anchor-outage disclosure.
func (s *Store) dexWindowBlocks(ctx context.Context, blk *BespokeBlock, source, since string, windowDays int) (string, error) {
	w, err := s.dexWindowKPIs(ctx, source, since, windowDays)
	if err != nil {
		return "", err
	}
	if w.trades == "0" || w.trades == "" {
		return "", nil
	}
	return w.xlmUnvalued, s.dexWindowParts(ctx, blk, source, since, windowDays, w)
}

// dexWindowParts appends the sub-parts of a window that has trades.
func (s *Store) dexWindowParts(ctx context.Context, blk *BespokeBlock, source, since string, windowDays int, w dexWindowTotals) error {
	raw := dexRawWindowOK(source, windowDays)
	pairs := w.pairs
	blk.KPIs = append(blk.KPIs,
		BespokeKPI{Label: fmt.Sprintf("USD volume (%dd)", windowDays), Value: w.vol, Unit: "USD", Hint: dexVolumeKPIHint(windowDays, w.xlmUnvalued)},
		BespokeKPI{Label: fmt.Sprintf("Trades (%dd)", windowDays), Value: w.trades},
	)
	var takersZero bool
	if raw {
		var err error
		if takersZero, err = s.dexRawKPIs(ctx, blk, source, since, windowDays, &pairs); err != nil {
			return err
		}
	}
	if pairs != "" {
		blk.KPIs = append(blk.KPIs,
			BespokeKPI{Label: fmt.Sprintf("Active pairs (%dd)", windowDays), Value: pairs, Hint: "distinct base/quote pairs traded in the window"})
	}

	if err := s.dexActivitySeries(ctx, blk, source, since, windowDays); err != nil {
		return err
	}
	if err := s.dexTopPairSeries(ctx, blk, source, since, windowDays); err != nil {
		return err
	}
	if err := s.dexPairBreakdown(ctx, blk, source, since, windowDays); err != nil {
		return err
	}
	if err := s.dexPairTables(ctx, blk, source, since, windowDays, raw); err != nil {
		return err
	}
	s.dexOmissionNotes(blk, windowDays, raw, takersZero)
	return nil
}

// dexVolumeKPIHint states the KPI's derivation. A non-empty xlmUnvalued
// makes the 24h figure a named lower bound (AGENTS.md money rule).
func dexVolumeKPIHint(windowDays int, xlmUnvalued string) string {
	if windowDays == 1 && xlmUnvalued != "" {
		return "LOWER BOUND: summed usd_volume of priced trades only; excludes " + xlmUnvalued + " XLM of unpriced XLM-denominated legs that were never priced at trade time"
	}
	if windowDays == 1 {
		return "summed usd_volume of priced trades over the trailing 24 hours"
	}
	return fmt.Sprintf("summed usd_volume of priced trades over the %d complete UTC days before today", windowDays)
}

// dexWindowTotals is one window KPI row. pairs is empty at 24h;
// xlmUnvalued is set only at 24h (see dexXLMLegUnvalued).
type dexWindowTotals struct {
	vol, trades, pairs, xlmUnvalued string
}

// dexWindowKPIs runs the window KPI query; at 24h pairs comes back empty and the raw KPI query fills it.
func (s *Store) dexWindowKPIs(ctx context.Context, source, since string, windowDays int) (dexWindowTotals, error) {
	var w dexWindowTotals
	var err error
	if windowDays == 1 {
		err = s.db.QueryRowContext(ctx, dexWindowKPIQuery(windowDays), source, since).Scan(&w.vol, &w.trades, &w.xlmUnvalued)
	} else {
		err = s.db.QueryRowContext(ctx, dexWindowKPIQuery(windowDays), source, since).Scan(&w.vol, &w.trades, &w.pairs)
	}
	if err != nil {
		return dexWindowTotals{}, fmt.Errorf("timescale: bespokeDEX window KPIs: %w", err)
	}
	return w, nil
}

// dexRawKPIs adds the raw-trades KPI cards (unique traders, avg trade size, 24h active-pairs fallback); takersZero reports a window with NO
// taker-stamped trades so dexOmissionNotes can disclose it.
func (s *Store) dexRawKPIs(ctx context.Context, blk *BespokeBlock, source, since string, windowDays int, pairs *string) (takersZero bool, err error) {
	var takers, priced, rawPairs, avg string
	err = s.db.QueryRowContext(ctx, dexRawKPIQuery(windowDays), source, since).
		Scan(&takers, &priced, &rawPairs, &avg)
	if err != nil {
		return false, fmt.Errorf("timescale: bespokeDEX raw KPIs: %w", err)
	}
	if *pairs == "" {
		*pairs = rawPairs
	}
	if takers != "0" {
		blk.KPIs = append(blk.KPIs,
			BespokeKPI{Label: fmt.Sprintf("Unique traders (%dd)", windowDays), Value: takers, Hint: "distinct taker addresses across the window's trades"})
	}
	if priced != "0" {
		blk.KPIs = append(blk.KPIs,
			BespokeKPI{Label: fmt.Sprintf("Avg trade size (%dd)", windowDays), Value: avg, Unit: "USD", Hint: dexAvgTradeHint(windowDays)})
	}
	return takers == "0", nil
}

// dexAvgTradeHint states the average's derivation: raw trades.usd_volume
// sum ÷ priced trades, the same numerator as the volume KPI.
func dexAvgTradeHint(int) string {
	return "summed usd_volume of priced trades ÷ priced trades (exact NUMERIC division; unpriced trades excluded from both sides)"
}

// dexActivitySeries appends the volume and trade-count series, plus unique traders when raw is servable and the rows carry takers.
func (s *Store) dexActivitySeries(ctx context.Context, blk *BespokeBlock, source, since string, windowDays int) error {
	volPts, tradePts, err := s.scanDexActivitySeries(ctx, dexActivitySeriesQuery(windowDays), source, since)
	if err != nil {
		return err
	}
	if len(volPts) > 0 {
		blk.Series = append(blk.Series, BespokeSeries{Name: dexSeriesVolume, Unit: "USD", Points: volPts})
	}
	if len(tradePts) > 0 {
		blk.Series = append(blk.Series, BespokeSeries{Name: dexSeriesTrades, Unit: "trades", Points: tradePts})
	}
	if !dexRawWindowOK(source, windowDays) {
		return nil
	}
	traderPts, err := s.scanDailySeries(ctx, dexTradersSeriesQuery(windowDays), source, since)
	if err != nil {
		return err
	}
	if len(traderPts) > 0 {
		blk.Series = append(blk.Series, BespokeSeries{Name: dexSeriesTraders, Unit: "traders", Points: traderPts})
	}
	return nil
}

// scanDexActivitySeries scans (bucket, vol, trades) rows into the two
// activity point slices.
func (s *Store) scanDexActivitySeries(ctx context.Context, query string, args ...any) (volPts, tradePts []BespokeSeriesPt, err error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("timescale: bespokeDEX activity series: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var date, vol, trades string
		if err := rows.Scan(&date, &vol, &trades); err != nil {
			return nil, nil, fmt.Errorf("timescale: bespokeDEX activity series scan: %w", err)
		}
		volPts = append(volPts, BespokeSeriesPt{Date: date, Value: vol})
		tradePts = append(tradePts, BespokeSeriesPt{Date: date, Value: trades})
	}
	return volPts, tradePts, rows.Err()
}

// dexTopPairSeries appends one "Top pairs · <PAIR>" series per top-5 pair,
// ordered by window volume descending (the query's row order).
func (s *Store) dexTopPairSeries(ctx context.Context, blk *BespokeBlock, source, since string, windowDays int) error {
	rows, err := s.db.QueryContext(ctx, dexTopPairsSeriesQuery(windowDays), source, since)
	if err != nil {
		return fmt.Errorf("timescale: bespokeDEX top-pair series: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var (
		out     []BespokeSeries
		lastKey string
	)
	for rows.Next() {
		var base, quote string
		var pt BespokeSeriesPt
		if err := rows.Scan(&base, &quote, &pt.Date, &pt.Value); err != nil {
			return fmt.Errorf("timescale: bespokeDEX top-pair series scan: %w", err)
		}
		key := base + "\x00" + quote
		if len(out) == 0 || key != lastKey {
			out = append(out, BespokeSeries{Name: dexTopPairPrefix + dexPairLabel(base, quote), Unit: "USD"})
			lastKey = key
		}
		out[len(out)-1].Points = append(out[len(out)-1].Points, pt)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("timescale: bespokeDEX top-pair series rows: %w", err)
	}
	blk.Series = append(blk.Series, out...)
	return nil
}

// dexPairBreakdown appends the volume-by-pair composition (top 8 +
// "Others") behind the page's donut.
func (s *Store) dexPairBreakdown(ctx context.Context, blk *BespokeBlock, source, since string, windowDays int) error {
	rows, err := s.db.QueryContext(ctx, dexPairBreakdownQuery(windowDays), source, since)
	if err != nil {
		return fmt.Errorf("timescale: bespokeDEX pair breakdown: %w", err)
	}
	defer func() { _ = rows.Close() }()
	bd := BespokeBreakdown{Title: "Volume by pair", Unit: "USD"}
	for rows.Next() {
		var base, quote string
		var row BespokeBreakdownRow
		if err := rows.Scan(&base, &quote, &row.Value, &row.Count); err != nil {
			return fmt.Errorf("timescale: bespokeDEX pair breakdown scan: %w", err)
		}
		if base == "" && quote == "" {
			row.Label = "Others"
		} else {
			row.Label = dexPairLabel(base, quote)
		}
		bd.Rows = append(bd.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("timescale: bespokeDEX pair breakdown rows: %w", err)
	}
	if len(bd.Rows) > 0 {
		blk.Breakdowns = append(blk.Breakdowns, bd)
	}
	return nil
}

// dexPairTables appends the top-pairs table and (when raw is servable)
// the largest-trades table.
func (s *Store) dexPairTables(ctx context.Context, blk *BespokeBlock, source, since string, windowDays int, raw bool) error {
	if err := s.dexTopPairsTable(ctx, blk, source, since, windowDays); err != nil {
		return err
	}
	if raw {
		return s.dexLargestTradesTable(ctx, blk, source, since, windowDays)
	}
	return nil
}

// dexTopPairsTable appends the top-25 pairs table; raw asset ids keep their own columns (the explorer links contract ids).
func (s *Store) dexTopPairsTable(ctx context.Context, blk *BespokeBlock, source, since string, windowDays int) error {
	rows, err := s.db.QueryContext(ctx, dexTopPairsTableQuery(windowDays), source, since)
	if err != nil {
		return fmt.Errorf("timescale: bespokeDEX top pairs table: %w", err)
	}
	defer func() { _ = rows.Close() }()
	tbl := BespokeTable{
		Title:   "Top pairs by USD volume",
		Columns: []string{"Pair", "Base", "Quote", "Trades", "USD volume", "Base turnover"},
	}
	for rows.Next() {
		var base, quote, trades, vol, baseVol string
		if err := rows.Scan(&base, &quote, &trades, &vol, &baseVol); err != nil {
			return fmt.Errorf("timescale: bespokeDEX top pairs table scan: %w", err)
		}
		tbl.Rows = append(tbl.Rows, []string{dexPairLabel(base, quote), base, quote, trades, vol, baseVol})
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("timescale: bespokeDEX top pairs table rows: %w", err)
	}
	if len(tbl.Rows) > 0 {
		blk.Tables = append(blk.Tables, tbl)
	}
	return nil
}

// dexLargestTradesTable appends the top-10 trades by USD volume; amounts are per-asset base-unit NUMERIC strings (ADR-0003).
func (s *Store) dexLargestTradesTable(ctx context.Context, blk *BespokeBlock, source, since string, windowDays int) error {
	rows, err := s.db.QueryContext(ctx, dexLargestTradesQuery(windowDays), source, since)
	if err != nil {
		return fmt.Errorf("timescale: bespokeDEX largest trades: %w", err)
	}
	defer func() { _ = rows.Close() }()
	tbl := BespokeTable{
		Title:   "Largest trades",
		Columns: []string{"Pair", "Base amount", "Quote amount", "USD volume", "Tx", "Date"},
	}
	for rows.Next() {
		var base, quote, baseAmt, quoteAmt, usd, txHash, day string
		if err := rows.Scan(&base, &quote, &baseAmt, &quoteAmt, &usd, &txHash, &day); err != nil {
			return fmt.Errorf("timescale: bespokeDEX largest trades scan: %w", err)
		}
		tbl.Rows = append(tbl.Rows, []string{dexPairLabel(base, quote), baseAmt, quoteAmt, usd, txHash, day})
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("timescale: bespokeDEX largest trades rows: %w", err)
	}
	if len(tbl.Rows) > 0 {
		blk.Tables = append(blk.Tables, tbl)
	}
	return nil
}

// dexSinceTotalsKPIs appends the totals with the rollup floor date in the label (the floor is NOT genesis; see dexSinceTotalsQuery).
func (s *Store) dexSinceTotalsKPIs(ctx context.Context, blk *BespokeBlock, source string) error {
	var vol, trades, floor string
	err := s.db.QueryRowContext(ctx, dexSinceTotalsQuery(), source).Scan(&vol, &trades, &floor)
	if err != nil {
		return fmt.Errorf("timescale: bespokeDEX since totals: %w", err)
	}
	if trades == "0" || trades == "" || floor == "" {
		return nil
	}
	blk.KPIs = append(blk.KPIs,
		BespokeKPI{Label: "Volume since " + floor, Value: vol, Unit: "USD", Hint: "summed USD volume across the daily rollup's full coverage"},
		BespokeKPI{Label: "Trades since " + floor, Value: trades, Hint: "total trades across the daily rollup's full coverage"},
	)
	blk.Notes = append(blk.Notes,
		fmt.Sprintf("'Since %s' totals cover the daily volume rollup's materialized range — %s is the rollup's coverage floor, not protocol genesis; raw trade history can extend earlier (SDEX raw trades reach back to 2018).", floor, floor),
	)
	return nil
}

// dexOmissionNotes appends notes for unservable surfaces; the trader-metrics note is DATA-DRIVEN (takersZero), as a static per-source claim
// would go stale once a decoder starts stamping takers.
func (s *Store) dexOmissionNotes(blk *BespokeBlock, windowDays int, raw, takersZero bool) {
	if raw && takersZero {
		blk.Notes = append(blk.Notes, fmt.Sprintf(
			"Unique-trader metrics are unavailable at the %dd window: none of its trades carries a taker address (rows ingested before the venue's decoder captured takers — e.g. soroswap rows predating 2026-07-30 — are not attributed unless re-derived).", windowDays,
		))
	}
	if !raw {
		blk.Notes = append(blk.Notes,
			fmt.Sprintf("Unique traders, average trade size and largest trades are omitted on SDEX at the %dd window: they require a raw scan of the trades hypertable (~10.7M rows per week for SDEX; measured 45s at 7d). The 24h window serves them.", windowDays),
		)
	}
}
