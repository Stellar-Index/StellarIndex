package timescale

// Split out of protocol_bespoke.go (2026-07-30) so per-category visual
// suites can be built in parallel without colliding on one file. Shared
// types (BespokeBlock/KPI/Series/Table/Breakdown) + the dispatcher + the
// scan helpers stay in protocol_bespoke.go.
//
// ─── DEX/AMM bespoke analytics (soroswap / phoenix / aquarius / comet /
//     sdex) ───────────────────────────────────────────────────────────────
//
// Three data tiers, chosen per query so nothing scans the 300M+-row trades
// hypertable unbounded (every figure below ground-truthed READ-ONLY on r1,
// 2026-07-30):
//
//  1. dex_volume_by_pair_1d (migration 0064, materialized_only=true) — the
//     daily per-(source, pair) rollup. Backs the >1-day windows: KPIs,
//     daily volume/trade series, the pair breakdown, top-5 pair lines and
//     the top-pairs table. Row counts: sdex 3.13M rows (2.1M in 90d,
//     99,562 distinct pairs), aquarius 25,688, soroswap 6,476, phoenix
//     1,112, comet 229. sdex 90d timings after two deliberate shapes:
//     window KPI 0.67s (a hash-agg pair-count subquery — the naive
//     count(DISTINCT (base,quote)) row-comparison sort measured 6.0s) and
//     top-5 series 0.87s (top CTE + direct index join — re-aggregating the
//     CAGG through a grouped CTE measured 19.8s). AMM sources are all
//     ≤50ms on the same shapes.
//
//     HONESTY FLOOR: the CAGG's materialization starts 2026-03-18 (its
//     min(bucket) on r1) while raw sdex trades extend back to 2018 —
//     re-materializing genesis-wide was not done. So lifetime figures are
//     served as "since <floor date>" KPIs, never as "all-time".
//
//  2. source_volume_1h (migration 0068, real-time) — the per-(source,
//     hour) rollup. Backs the 24h window's hourly volume/trade series +
//     KPIs (the daily CAGG would collapse 24h into one partial point).
//
//  3. Raw `trades` — the ONLY place taker addresses and per-trade rows
//     live, so unique-trader metrics, the priced-trade count behind avg
//     trade size, the largest-trades table, and every 24h per-pair shape
//     come from here, window-bounded. Compression segments by (base,
//     quote, source), so per-source scans prune well: aquarius 90d (1.0M
//     rows) 1.0s, soroswap/phoenix/comet 90d ≤0.3s, sdex 24h (754k rows)
//     0.3–1.0s. sdex 7d measured 1.4s and extrapolates to ~15–20s at 90d,
//     so raw-derived surfaces are gated OFF for sdex beyond 7 days
//     (dexRawWindowOK) and the omission is Noted on the block.
//
// taker coverage (r1, full 90d): aquarius/phoenix/comet/sdex stamp taker
// on 100% of rows; soroswap stamps NONE (its decoder never captured the
// taker), so soroswap's trader metrics are omitted with an honest Note.
//
// All USD figures come ONLY from trades.usd_volume (or its CAGG sums) —
// never ad-hoc pricing. NULL-usd trades are excluded from USD sums and
// averages but still count toward trade totals. All division is exact
// NUMERIC (ADR-0003); rounding is round(x, 2) — never a float literal.

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

// dexRawWindowOK reports whether raw-trades-derived surfaces (unique
// traders, avg trade size, largest trades, the 24h per-pair shapes) are
// servable for source at this window. sdex is the one source big enough
// to price out: 754k rows/24h (~1s), 5.0M rows/7d (1.4s measured), ~15–20s
// extrapolated at 90d — so sdex serves raw-derived surfaces on the 24h/7d
// windows only. Every other DEX source is ≤1.0M rows even at 90d.
func dexRawWindowOK(source string, windowDays int) bool {
	return source != "sdex" || windowDays <= 7
}

// ─── query builders ──────────────────────────────────────────────────────
//
// Every windowed query binds source=$1 and the window=$2::interval; the
// since-totals query binds source=$1 only (it is deliberately unwindowed
// over the materialized-only CAGG — 0.23s for sdex's 3.13M rows on r1).

// dexActivitySeriesQuery returns the (bucket, usd_volume, trades) series
// at the window's grain: hourly from the real-time source_volume_1h CAGG
// at 24h (24 rows, 17ms on sdex), daily from dex_volume_by_pair_1d
// otherwise (87 rows, 0.43s on sdex 90d).
func dexActivitySeriesQuery(windowDays int) string {
	if windowDays == 1 {
		return `
			SELECT to_char(bucket, 'YYYY-MM-DD"T"HH24:00'), COALESCE(sum(sum_usd_priced),0)::text, COALESCE(sum(trade_count),0)::text
			FROM source_volume_1h WHERE source = $1 AND bucket > now() - $2::interval
			GROUP BY 1 ORDER BY 1 ASC`
	}
	return `
		SELECT to_char(bucket, 'YYYY-MM-DD'), COALESCE(sum(vol),0)::text, COALESCE(sum(trades),0)::text
		FROM dex_volume_by_pair_1d WHERE source = $1 AND bucket > now() - $2::interval
		GROUP BY 1 ORDER BY 1 ASC`
}

// dexWindowKPIQuery returns (usd_volume, trades, active_pairs) over the
// window. The >1d form reads the daily CAGG; the pair count is a hash-agg
// subquery, NOT count(DISTINCT (base,quote)) — the row-comparison sort of
// the latter measured 6.0s on sdex 90d vs 0.67s for this shape (99,562
// pairs). The 24h form reads source_volume_1h, which has no pair
// dimension — the caller fills active-pairs from the raw KPI query
// (dexRawWindowOK is always true at 24h).
func dexWindowKPIQuery(windowDays int) string {
	if windowDays == 1 {
		return `
			SELECT round(COALESCE(sum(sum_usd_priced),0),2)::text, COALESCE(sum(trade_count),0)::text
			FROM source_volume_1h WHERE source = $1 AND bucket > now() - $2::interval`
	}
	return `
		SELECT round(COALESCE(sum(vol),0),2)::text, COALESCE(sum(trades),0)::text,
		       (SELECT count(*) FROM (
		          SELECT 1 FROM dex_volume_by_pair_1d
		           WHERE source = $1 AND bucket > now() - $2::interval
		           GROUP BY base_asset, quote_asset) q)::text
		FROM dex_volume_by_pair_1d WHERE source = $1 AND bucket > now() - $2::interval`
}

// dexRawKPIQuery returns (unique_takers, priced_trades, active_pairs,
// avg_trade_usd) from raw trades over the window. The average is exact
// NUMERIC division of the USD-volume sum by the PRICED trade count
// (ADR-0003; NULL-usd trades are excluded from both sides), rounded to
// cents. aquarius 90d (the heaviest AMM: 1.0M rows) 1.0s; sdex gated to
// ≤7d by dexRawWindowOK.
func dexRawKPIQuery() string {
	return `
		SELECT count(DISTINCT taker)::text, count(usd_volume)::text,
		       count(DISTINCT (base_asset, quote_asset))::text,
		       COALESCE(round(sum(usd_volume) / NULLIF(count(usd_volume),0), 2), 0)::text
		FROM trades WHERE source = $1 AND ts > now() - $2::interval`
}

// dexTradersSeriesQuery returns the per-bucket unique-taker series from
// raw trades at the window's grain (hourly at 24h, daily otherwise —
// bridgeSeriesGrain). count(DISTINCT taker) needs the raw rows: no CAGG
// carries takers. 91 rows / 0.47s on aquarius 90d.
func dexTradersSeriesQuery(windowDays int) string {
	trunc, format := bridgeSeriesGrain(windowDays)
	return `
		SELECT to_char(date_trunc('` + trunc + `', ts), '` + format + `'), count(DISTINCT taker)::text
		FROM trades WHERE source = $1 AND ts > now() - $2::interval AND taker IS NOT NULL
		GROUP BY 1 ORDER BY 1 ASC`
}

// dexPairBreakdownQuery returns the window's volume-by-pair composition:
// the top 8 pairs by USD volume plus one fold-the-rest row whose pair
// columns are empty (the caller labels it "Others"). Ranking and folding
// happen in SQL so sdex's 99k window pairs never cross the wire (9 rows /
// 0.84s on sdex 90d; 9 rows / 13ms on aquarius). The 24h form computes
// the same shape from raw trades (25 rows scanned shape, 0.51s on sdex
// 24h) because the daily CAGG cannot bucket a 24h window.
func dexPairBreakdownQuery(windowDays int) string {
	var p string
	if windowDays == 1 {
		p = `SELECT base_asset, quote_asset, COALESCE(sum(usd_volume),0) AS vol, count(*) AS n
		    FROM trades
		   WHERE source = $1 AND ts > now() - $2::interval
		   GROUP BY 1, 2`
	} else {
		p = `SELECT base_asset, quote_asset, COALESCE(sum(vol),0) AS vol, sum(trades) AS n
		    FROM dex_volume_by_pair_1d
		   WHERE source = $1 AND bucket > now() - $2::interval
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

// dexTopPairsSeriesQuery returns the top-5-pairs-by-window-volume series:
// (base, quote, bucket, usd) rows ordered by each pair's window volume
// descending, then bucket. The >1d form joins the daily CAGG directly
// against the top-5 CTE — CAGG rows are already unique per (source, pair,
// bucket) (materialized_only=true), and re-aggregating them through a
// grouped CTE first measured 19.8s on sdex 90d vs 0.87s for this shape
// (364 rows). The 24h form buckets raw trades hourly.
func dexTopPairsSeriesQuery(windowDays int) string {
	if windowDays == 1 {
		return `
			WITH p AS (
			  SELECT base_asset, quote_asset, date_trunc('hour', ts) AS bucket, COALESCE(sum(usd_volume),0) AS vol
			    FROM trades
			   WHERE source = $1 AND ts > now() - $2::interval
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
		   WHERE source = $1 AND bucket > now() - $2::interval
		   GROUP BY 1, 2 ORDER BY total DESC NULLS LAST LIMIT 5)
		SELECT t.base_asset, t.quote_asset, to_char(t.bucket, 'YYYY-MM-DD'), COALESCE(t.vol,0)::text
		FROM dex_volume_by_pair_1d t JOIN top USING (base_asset, quote_asset)
		WHERE t.source = $1 AND t.bucket > now() - $2::interval
		ORDER BY top.total DESC NULLS LAST, t.base_asset, t.quote_asset, t.bucket ASC`
}

// dexTopPairsTableQuery returns the top-25 pairs table rows (base, quote,
// trades, usd, base turnover). 25 rows / 0.79s on sdex 90d. The 24h form
// reads raw trades for the same reason as the breakdown.
func dexTopPairsTableQuery(windowDays int) string {
	if windowDays == 1 {
		return `
			SELECT base_asset, quote_asset,
			       count(*)::text, round(COALESCE(sum(usd_volume),0),2)::text, COALESCE(sum(base_amount),0)::text
			  FROM trades
			 WHERE source = $1 AND ts > now() - $2::interval
			 GROUP BY base_asset, quote_asset
			 ORDER BY sum(usd_volume) DESC NULLS LAST, count(*) DESC LIMIT 25`
	}
	return `
		SELECT base_asset, quote_asset,
		       COALESCE(sum(trades),0)::text, round(COALESCE(sum(vol),0),2)::text, COALESCE(sum(base_vol),0)::text
		  FROM dex_volume_by_pair_1d
		 WHERE source = $1 AND bucket > now() - $2::interval
		 GROUP BY base_asset, quote_asset
		 ORDER BY sum(vol) DESC NULLS LAST, sum(trades) DESC LIMIT 25`
}

// dexLargestTradesQuery returns the window's top-10 trades by USD volume
// from raw trades (per-trade rows exist nowhere else). Amounts stay
// per-asset base-unit NUMERIC strings; ordering is on the raw usd_volume.
// 0.26s on aquarius 90d / sdex 24h; sdex gated to ≤7d by dexRawWindowOK.
func dexLargestTradesQuery() string {
	return `
		SELECT base_asset, quote_asset, base_amount::text, quote_amount::text,
		       round(usd_volume,2)::text, tx_hash, to_char(ts, 'YYYY-MM-DD')
		FROM trades WHERE source = $1 AND ts > now() - $2::interval AND usd_volume IS NOT NULL
		ORDER BY usd_volume DESC LIMIT 10`
}

// dexSinceTotalsQuery returns the source's lifetime-within-the-rollup
// totals: (usd_volume, trades, floor_date) over ALL materialized buckets.
// Deliberately unwindowed (0.23s over sdex's 3.13M CAGG rows on r1) and
// deliberately NOT called "all-time": the rollup's materialization floor
// is 2026-03-18 on r1 while raw sdex history reaches 2018 — the floor
// date is served with the KPI so the label stays honest.
func dexSinceTotalsQuery() string {
	return `
		SELECT round(COALESCE(sum(vol),0),2)::text, COALESCE(sum(trades),0)::text, COALESCE(min(bucket)::date::text, '')
		FROM dex_volume_by_pair_1d WHERE source = $1`
}

// ─── pair labelling ──────────────────────────────────────────────────────

// dexSACSymbols lazily builds the token-contract → verified-ticker map the
// pair labels resolve through: the native SAC → "XLM" plus every verified
// currency's Stellar issuance (classic assets via deterministic SAC
// derivation, Soroban-native tokens by their own contract id). Pure
// in-memory derivation off the embedded catalogue — no DB read, built
// once per process.
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
		case canonical.AssetNative, canonical.AssetFiat, canonical.AssetCrypto, canonical.AssetRWA:
			// native handled above; fiat/crypto/RWA ids never appear as
			// trade-leg token contracts.
		}
	}
	return m
})

// dexAssetLabel renders one canonical trade-leg asset id as a short human
// symbol: "native" → XLM, "CODE-G…" → CODE, a token contract → its
// verified ticker when the catalogue knows it, else an honest truncated
// contract ("CBZ4…N2PJ") — never a guessed symbol.
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

// bespokeDEX builds the DEX/AMM bespoke block. See the file doc for the
// three data tiers and the honesty rules each sub-part encodes.
func (s *Store) bespokeDEX(ctx context.Context, source string, windowDays int) (*BespokeBlock, error) {
	since := fmt.Sprintf("%d days", windowDays)
	blk := &BespokeBlock{
		Category: "dex",
		Notes: []string{
			"USD figures are sums of the ingest-time trades.usd_volume valuation only — never ad-hoc pricing. Trades whose quote never resolved to a USD price are excluded from USD sums and averages but still count toward trade totals.",
			"Pairs are labelled by verified token tickers (native XLM plus the verified-currency catalogue's SAC/token addresses); an unverified token shows a truncated contract id, never a guessed symbol. Base/quote amounts are token base units at per-asset decimals, not USD.",
		},
	}

	if err := s.dexWindowBlocks(ctx, blk, source, since, windowDays); err != nil {
		return nil, err
	}
	if err := s.dexSinceTotalsKPIs(ctx, blk, source); err != nil {
		return nil, err
	}

	// Per-source captured-liquidity augments (reserves / net-flow depth /
	// staking / skim) — independent of trade volume so a quiet window still
	// surfaces depth, and empty-safe until the decoders have captured
	// anything on r1.
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

// dexWindowBlocks fills every window-scoped sub-part: KPIs, the activity +
// traders + top-pair series, the pair breakdown, and the two tables. A
// window with zero trades adds nothing (the block degrades to the
// since-totals + augments).
func (s *Store) dexWindowBlocks(ctx context.Context, blk *BespokeBlock, source, since string, windowDays int) error {
	vol, trades, pairs, err := s.dexWindowKPIs(ctx, source, since, windowDays)
	if err != nil {
		return err
	}
	if trades == "0" || trades == "" {
		return nil
	}

	raw := dexRawWindowOK(source, windowDays)
	blk.KPIs = append(blk.KPIs,
		BespokeKPI{Label: fmt.Sprintf("USD volume (%dd)", windowDays), Value: vol, Unit: "USD", Hint: "summed usd_volume of priced trades over the window"},
		BespokeKPI{Label: fmt.Sprintf("Trades (%dd)", windowDays), Value: trades},
	)
	if raw {
		if err := s.dexRawKPIs(ctx, blk, source, since, windowDays, &pairs); err != nil {
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
	s.dexOmissionNotes(blk, source, windowDays, raw)
	return nil
}

// dexWindowKPIs runs the window KPI query for the grain. The 24h form has
// no pair dimension (source_volume_1h) — pairs comes back empty and is
// filled by the raw KPI query.
func (s *Store) dexWindowKPIs(ctx context.Context, source, since string, windowDays int) (vol, trades, pairs string, err error) {
	if windowDays == 1 {
		err = s.db.QueryRowContext(ctx, dexWindowKPIQuery(windowDays), source, since).Scan(&vol, &trades)
	} else {
		err = s.db.QueryRowContext(ctx, dexWindowKPIQuery(windowDays), source, since).Scan(&vol, &trades, &pairs)
	}
	if err != nil {
		return "", "", "", fmt.Errorf("timescale: bespokeDEX window KPIs: %w", err)
	}
	return vol, trades, pairs, nil
}

// dexRawKPIs adds the raw-trades-derived KPI cards: unique traders (when
// the source captures takers — soroswap does not), avg trade size over
// priced trades, and the active-pairs fallback for the 24h grain.
func (s *Store) dexRawKPIs(ctx context.Context, blk *BespokeBlock, source, since string, windowDays int, pairs *string) error {
	var takers, priced, rawPairs, avg string
	err := s.db.QueryRowContext(ctx, dexRawKPIQuery(), source, since).
		Scan(&takers, &priced, &rawPairs, &avg)
	if err != nil {
		return fmt.Errorf("timescale: bespokeDEX raw KPIs: %w", err)
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
			BespokeKPI{Label: fmt.Sprintf("Avg trade size (%dd)", windowDays), Value: avg, Unit: "USD", Hint: "window USD volume ÷ priced trades (exact NUMERIC division; unpriced trades excluded from both sides)"})
	}
	return nil
}

// dexActivitySeries appends the volume + trade-count series (one scan) and
// the unique-traders series (raw, when servable and the source captures
// takers).
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
		return s.dexLargestTradesTable(ctx, blk, source, since)
	}
	return nil
}

// dexTopPairsTable appends the top-25 pairs table. Raw asset ids stay in
// their own columns (the explorer links contract ids); the pair label
// leads the row.
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

// dexLargestTradesTable appends the window's top-10 trades by USD volume.
// Amounts are per-asset base-unit NUMERIC strings (ADR-0003); the tx hash
// column links to the transaction page client-side.
func (s *Store) dexLargestTradesTable(ctx context.Context, blk *BespokeBlock, source, since string) error {
	rows, err := s.db.QueryContext(ctx, dexLargestTradesQuery(), source, since)
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

// dexSinceTotalsKPIs appends the lifetime-within-the-rollup totals with
// the honest floor date in the label (see dexSinceTotalsQuery — the
// rollup floor is NOT protocol genesis; raw history can extend earlier).
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

// dexOmissionNotes appends the honest omission notes for surfaces this
// source/window combination cannot serve.
func (s *Store) dexOmissionNotes(blk *BespokeBlock, source string, windowDays int, raw bool) {
	if source == "soroswap" {
		blk.Notes = append(blk.Notes,
			"Unique-trader metrics are unavailable for Soroswap: its trade decoder does not capture the taker address (0% taker coverage on r1), so trader counts would be dishonest zeros rather than data.",
		)
	}
	if !raw {
		blk.Notes = append(blk.Notes,
			fmt.Sprintf("Unique traders, average trade size and largest trades are omitted on SDEX at the %dd window: they require a raw scan of the trades hypertable (~5M rows per week for SDEX; measured 1.4s at 7d, extrapolating to ~15–20s at 90d). The 24h and 7d windows serve them.", windowDays),
		)
	}
}
