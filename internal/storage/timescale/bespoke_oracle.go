package timescale

// Split out of protocol_bespoke.go (2026-07-30) so per-category visual
// suites can be built in parallel without colliding on one file. Shared
// types (BespokeBlock/KPI/Series/Table/Breakdown) + the dispatcher + the
// scan helpers stay in protocol_bespoke.go.

// ─── Oracle bespoke analytics (reflector-dex/cex/fx, redstone, band) ─────
//
// Everything here is COUNTS + TIMESTAMPS over oracle_updates scoped by
// source = the protocol page's name (which matches oracle_updates.source
// exactly for all five oracle pages — verified on r1 2026-07-30). No price
// averaging, rescaling, or aggregation happens on this page: aggregated
// pricing is the aggregator's domain (ADR-0006 / docs/methodology), and
// oracle sources never feed VWAP anyway (external.Registry class policy).
// The one place a price appears — the "Latest feed prices" table — shows
// the raw observed integer verbatim with its declared decimals column.
//
// Batch semantics: some sources (redstone in particular — one "REDSTONE"
// event per batch push containing every updated feed) land one ROW PER
// FEED per publication, so update counts weight feeds, not transactions.
// The median publish interval is therefore measured between DISTINCT
// publication timestamps, not rows — row-to-row gaps within one batch are
// zero and would report a dishonest "0s cadence".
//
// History note: oracle_updates carries NO retention (migration 0040
// removed the original 90-day policy), so "all-time" figures cover every
// retained observation — history begins at each source's first ingested
// observation (e.g. reflector-* 2026-03-11, redstone 2025-09-09 on r1),
// NOT at protocol genesis.
//
// Every windowed query is bounded by ts > now() - $N::interval (1-day
// hypertable chunks make this a cheap chunk-excluded scan); the two
// all-time reads are narrow index counts. The whole block builds under
// the window-keyed protocol-detail TTL cache.

import (
	"context"
	"fmt"
	"strconv"
)

// oracleSeriesNameUpdates is the window-stable name of the total update
// series; the grain (hourly at the 24h window, daily otherwise) lives in
// the point timestamps, mirroring the bridge/CCTP series convention.
const oracleSeriesNameUpdates = "Updates"

// oracleBreakdownTopN caps the "Updates by feed" donut at the top feeds,
// folding the tail into one honest "Others" slice.
const oracleBreakdownTopN = 10

// oracleWindowKPIQuery returns the windowed headline counts: updates,
// distinct feeds, mean updates/day, and the freshest-update age (now minus
// the newest publication ts in the window, truncated to seconds).
// $1 = source, $2 = windowDays, $3 = interval.
func oracleWindowKPIQuery() string {
	return `
		SELECT count(*)::text,
		       count(DISTINCT (asset, quote))::text,
		       CASE WHEN $2 > 0 THEN round(count(*)::numeric / $2, 1)::text ELSE '0' END,
		       COALESCE(date_trunc('second', now() - max(ts))::text, '—')
		FROM oracle_updates
		WHERE source = $1 AND ts > now() - $3::interval`
}

// oracleMedianIntervalQuery returns the median gap between successive
// DISTINCT publication timestamps in the window (see the batch-semantics
// file doc — row-to-row gaps within one batch push are zero). NULL (fewer
// than two distinct timestamps) renders as an honest em dash.
// $1 = source, $2 = interval.
func oracleMedianIntervalQuery() string {
	return `
		WITH p AS (
		  SELECT DISTINCT ts FROM oracle_updates
		   WHERE source = $1 AND ts > now() - $2::interval
		), d AS (SELECT ts - lag(ts) OVER (ORDER BY ts) AS gap FROM p)
		SELECT COALESCE(date_trunc('second', percentile_cont(0.5) WITHIN GROUP (ORDER BY gap))::text, '—')
		FROM d WHERE gap IS NOT NULL`
}

// oracleAllTimeKPIQuery returns the retained-history totals: every stored
// update for the source plus the first observation date. Deliberately NOT
// window-bounded — but see the file doc: this is served-tier retained
// history (no retention since migration 0040), not protocol genesis.
// $1 = source.
func oracleAllTimeKPIQuery() string {
	return `
		SELECT count(*)::text, COALESCE(min(ts)::date::text, '—')
		FROM oracle_updates WHERE source = $1`
}

// oracleUpdatesSeriesQuery builds the total update-count series at the
// window's grain (hourly at 24h, daily otherwise). $1 = source, $2 = interval.
func oracleUpdatesSeriesQuery(windowDays int) string {
	trunc, format := bridgeSeriesGrain(windowDays)
	return `
		SELECT to_char(date_trunc('` + trunc + `', ts), '` + format + `'), count(*)::text
		FROM oracle_updates WHERE source = $1 AND ts > now() - $2::interval
		GROUP BY 1 ORDER BY 1 ASC`
}

// oraclePerFeedSeriesQuery builds the top-5-feeds-by-window-count series:
// (feed_label, bucket, count) rows ordered by each feed's window count
// descending, then bucket — folded into per-feed lines by
// collectKeyedSeries. $1 = source, $2 = interval.
func oraclePerFeedSeriesQuery(windowDays int) string {
	trunc, format := bridgeSeriesGrain(windowDays)
	return `
		WITH top AS (
		  SELECT asset, quote, count(*) AS n FROM oracle_updates
		   WHERE source = $1 AND ts > now() - $2::interval
		   GROUP BY 1, 2 ORDER BY n DESC, asset, quote LIMIT 5
		)
		SELECT u.asset || ' / ' || u.quote,
		       to_char(date_trunc('` + trunc + `', u.ts), '` + format + `'),
		       count(*)::text
		FROM oracle_updates u JOIN top t ON t.asset = u.asset AND t.quote = u.quote
		WHERE u.source = $1 AND u.ts > now() - $2::interval
		GROUP BY 1, 2, t.n
		ORDER BY t.n DESC, 1, 2 ASC`
}

// oracleFeedBreakdownQuery returns per-feed window update counts, count-
// descending; Go folds the tail beyond oracleBreakdownTopN into "Others".
// $1 = source, $2 = interval.
func oracleFeedBreakdownQuery() string {
	return `
		SELECT asset || ' / ' || quote, count(*)::text, count(*)
		FROM oracle_updates WHERE source = $1 AND ts > now() - $2::interval
		GROUP BY asset, quote ORDER BY count(*) DESC, 1 ASC`
}

// oracleFeedsTableQuery returns the top-15 feeds by window update count
// with each feed's last publication time. $1 = source, $2 = interval.
func oracleFeedsTableQuery() string {
	return `
		SELECT asset, quote, count(*)::text, to_char(max(ts), 'YYYY-MM-DD HH24:MI')
		FROM oracle_updates WHERE source = $1 AND ts > now() - $2::interval
		GROUP BY asset, quote ORDER BY count(*) DESC, asset, quote LIMIT 15`
}

// oracleLatestPricesQuery returns the pre-existing latest-observation
// table: one row per feed with the raw integer price shown verbatim at its
// declared decimals — no rescaling or averaging (see the file doc).
// $1 = source, $2 = interval.
func oracleLatestPricesQuery() string {
	return `
		SELECT asset,
		        quote,
		        COALESCE(price::text,'—'),
		        COALESCE(decimals::text,'—'),
		        to_char(ts, 'YYYY-MM-DD HH24:MI')
		   FROM (
		     SELECT DISTINCT ON (asset, quote) asset, quote, price, decimals, ts
		       FROM oracle_updates
		      WHERE source = $1 AND ts > now() - $2::interval
		      ORDER BY asset, quote, ts DESC
		   ) latest
		  ORDER BY ts DESC LIMIT 50`
}

// bespokeOracle builds the oracle bespoke block (reflector-dex/cex/fx,
// band, redstone) from oracle_updates scoped by source: update-cadence
// KPIs (window + retained-history), the total and per-feed update series,
// the updates-by-feed breakdown, the per-feed cadence table, and the
// latest raw-observation table.
func (s *Store) bespokeOracle(ctx context.Context, source string, windowDays int) (*BespokeBlock, error) {
	since := fmt.Sprintf("%d days", windowDays)
	blk := &BespokeBlock{
		Category: "oracle",
		Notes: []string{
			"Scoped to oracle_updates.source = the protocol's feed contract. Everything on this page is update COUNTS + TIMESTAMPS — no price averaging or aggregation happens here (aggregated pricing is the aggregator's domain, and oracle observations never feed VWAP). The latest-prices table shows the raw on-chain integer at the row's `decimals` scale, verbatim.",
			"Batch sources (redstone: one event per batch push containing every updated feed) land one row PER FEED per publication, so update counts weight feeds, not transactions; the median publish interval is measured between DISTINCT publication timestamps for the same reason.",
			"All-time figures cover every retained observation in the served tier — oracle_updates carries no retention (migration 0040) and history begins at this source's first ingested observation, not protocol genesis.",
		},
	}

	empty, err := s.oracleKPIs(ctx, blk, source, since, windowDays)
	if err != nil {
		return nil, err
	}
	if empty {
		return nil, nil // no updates for this feed in the window — omit, don't show zeros
	}
	if err := s.oracleSeriesBlocks(ctx, blk, source, since, windowDays); err != nil {
		return nil, err
	}
	if err := s.oracleFeedBreakdown(ctx, blk, source, since, windowDays); err != nil {
		return nil, err
	}
	return blk, s.oracleTables(ctx, blk, source, since, windowDays)
}

// oracleKPIs fills the headline cards; reports empty=true when the source
// had no update in the window (the caller omits the whole block).
func (s *Store) oracleKPIs(ctx context.Context, blk *BespokeBlock, source, since string, windowDays int) (bool, error) {
	var updates, feeds, perDay, freshAge string
	err := s.db.QueryRowContext(ctx, oracleWindowKPIQuery(), source, windowDays, since).
		Scan(&updates, &feeds, &perDay, &freshAge)
	if err != nil {
		return false, fmt.Errorf("timescale: bespokeOracle KPIs: %w", err)
	}
	if updates == "0" {
		return true, nil
	}

	var median string
	if err := s.db.QueryRowContext(ctx, oracleMedianIntervalQuery(), source, since).Scan(&median); err != nil {
		return false, fmt.Errorf("timescale: bespokeOracle median interval: %w", err)
	}
	var allUpdates, firstSeen string
	if err := s.db.QueryRowContext(ctx, oracleAllTimeKPIQuery(), source).Scan(&allUpdates, &firstSeen); err != nil {
		return false, fmt.Errorf("timescale: bespokeOracle all-time KPIs: %w", err)
	}

	blk.KPIs = append(blk.KPIs,
		BespokeKPI{Label: fmt.Sprintf("Updates (%dd)", windowDays), Value: updates},
		BespokeKPI{Label: "Distinct feeds", Value: feeds, Hint: "distinct asset/quote pairs seen in the window"},
		BespokeKPI{Label: "Updates / day", Value: perDay, Hint: "mean update cadence over the window"},
		BespokeKPI{Label: "Freshest update age", Value: freshAge, Hint: "now minus the most recent publication timestamp in the window (HH:MM:SS)"},
		BespokeKPI{Label: fmt.Sprintf("Median publish interval (%dd)", windowDays), Value: median, Hint: "median gap between successive DISTINCT publication timestamps in the window — batch pushes updating many feeds at once count as one publication"},
		BespokeKPI{Label: "All-time updates", Value: allUpdates, Hint: "every retained observation for this source in the served tier (no retention since migration 0040) — history begins at the source's first ingested observation, not protocol genesis"},
		BespokeKPI{Label: "Observed since", Value: firstSeen, Hint: "date of this source's first retained observation"},
	)
	return false, nil
}

// oracleSeriesBlocks appends the total update series plus one line per
// top-5 feed by window update count, all at the window's grain.
func (s *Store) oracleSeriesBlocks(ctx context.Context, blk *BespokeBlock, source, since string, windowDays int) error {
	series, err := s.scanDailySeries(ctx, oracleUpdatesSeriesQuery(windowDays), source, since)
	if err != nil {
		return err
	}
	if len(series) > 0 {
		blk.Series = append(blk.Series, BespokeSeries{Name: oracleSeriesNameUpdates, Unit: "updates", Points: series})
	}

	perFeed, err := s.collectKeyedSeries(ctx, oraclePerFeedSeriesQuery(windowDays), "updates",
		func(key string) string { return key }, source, since)
	if err != nil {
		return fmt.Errorf("timescale: bespokeOracle per-feed series: %w", err)
	}
	blk.Series = append(blk.Series, perFeed...)
	return nil
}

// oracleFeedBreakdown fills the updates-by-feed composition (top feeds +
// an aggregated "Others" slice) behind the donut.
func (s *Store) oracleFeedBreakdown(ctx context.Context, blk *BespokeBlock, source, since string, windowDays int) error {
	bd := BespokeBreakdown{Title: fmt.Sprintf("Updates by feed (%dd)", windowDays), Unit: "updates"}
	rows, err := s.db.QueryContext(ctx, oracleFeedBreakdownQuery(), source, since)
	if err != nil {
		return fmt.Errorf("timescale: bespokeOracle feed breakdown: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var row BespokeBreakdownRow
		if err := rows.Scan(&row.Label, &row.Value, &row.Count); err != nil {
			return fmt.Errorf("timescale: bespokeOracle feed breakdown scan: %w", err)
		}
		bd.Rows = append(bd.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("timescale: bespokeOracle feed breakdown rows: %w", err)
	}
	bd.Rows = foldBreakdownRows(bd.Rows, oracleBreakdownTopN)
	if len(bd.Rows) > 0 {
		blk.Breakdowns = append(blk.Breakdowns, bd)
	}
	return nil
}

// oracleTables fills the per-feed cadence table (counts + last update)
// and the pre-existing latest-raw-observation table.
func (s *Store) oracleTables(ctx context.Context, blk *BespokeBlock, source, since string, windowDays int) error {
	feeds, err := s.scanTable(ctx,
		BespokeTable{Title: "Feeds", Columns: []string{"Asset", "Quote", fmt.Sprintf("Updates (%dd)", windowDays), "Last update (UTC)"}},
		oracleFeedsTableQuery(), source, since)
	if err != nil {
		return err
	}
	if len(feeds.Rows) > 0 {
		blk.Tables = append(blk.Tables, feeds)
	}

	tbl, err := s.scanTable(ctx,
		BespokeTable{Title: "Latest feed prices", Columns: []string{"Asset", "Quote", "Price (raw)", "Decimals", "Updated"}},
		oracleLatestPricesQuery(), source, since)
	if err != nil {
		return err
	}
	if len(tbl.Rows) > 0 {
		blk.Tables = append(blk.Tables, tbl)
	}
	return nil
}

// collectKeyedSeries runs a (key, bucket, value) query and folds its
// ordered rows into one BespokeSeries per key, preserving the query's key
// order and naming each series via nameFor. Shared by the oracle per-feed
// lines and the yield per-direction lines (same package, defined once here).
func (s *Store) collectKeyedSeries(ctx context.Context, query, unit string, nameFor func(string) string, args ...any) ([]BespokeSeries, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("timescale: bespoke keyed series: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var (
		out     []BespokeSeries
		lastKey string
	)
	for rows.Next() {
		var key string
		var pt BespokeSeriesPt
		if err := rows.Scan(&key, &pt.Date, &pt.Value); err != nil {
			return nil, fmt.Errorf("timescale: bespoke keyed series scan: %w", err)
		}
		if len(out) == 0 || key != lastKey {
			out = append(out, BespokeSeries{Name: nameFor(key), Unit: unit})
			lastKey = key
		}
		out[len(out)-1].Points = append(out[len(out)-1].Points, pt)
	}
	return out, rows.Err()
}

// foldBreakdownRows keeps the first topN rows (the query orders them
// count-descending) and folds the remainder into one "Others" row whose
// Value/Count are the tail's summed counts. Intended for COUNT-weighted
// breakdowns where Value is the decimal rendering of Count — never for
// token-amount breakdowns (amounts across feeds/vaults are different
// units and must not be summed).
func foldBreakdownRows(rows []BespokeBreakdownRow, topN int) []BespokeBreakdownRow {
	if len(rows) <= topN {
		return rows
	}
	out := rows[:topN:topN]
	var tail int64
	for _, r := range rows[topN:] {
		tail += r.Count
	}
	return append(out, BespokeBreakdownRow{Label: "Others", Value: strconv.FormatInt(tail, 10), Count: tail})
}
