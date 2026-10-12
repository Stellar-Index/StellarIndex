// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// curated_rwa_published_series caches the totals a third-party curator PUBLISHES about tokenized
// real-world assets, for the RWA curated arm (synced by `curated-rwa-sync`). It is separate from
// rwa_curated_directory because the per-asset list lives in private uploads; only the latest RESULT
// of the curator's public queries is readable.
//
// A row is the curator's own arithmetic over private inputs (no per-asset breakdown, price or
// market), served under its name beside the verified set, never inside it.
//
// TWO CLOCKS. `executed_at` is the CURATOR's, so a figure is only as fresh as that. `observed_at` is
// OURS; the recognition bound ages on it, spliced into the SQL so no caller can read a stale row as
// fresh. A sync replaces a curator's rows PER SERIES in one transaction, so a series shares both.

// Series names, shared by the writer and the reader.
const (
	// CuratedRWASeriesMonthlyTotal is one row per month_end (subclass ''): the headline total.
	CuratedRWASeriesMonthlyTotal = "rwa_mcap_monthly"
	// CuratedRWASeriesMonthlyBySubclass is one row per (month_end, subclass), the curator's own split.
	CuratedRWASeriesMonthlyBySubclass = "rwa_mcap_monthly_by_subclass"
)

// CuratedRWAPublishedRow is one point of a published series, as the curator's query printed it.
type CuratedRWAPublishedRow struct {
	Series string
	// MonthEnd is the last day of the bucketed month, midnight UTC, stored as a date.
	MonthEnd time.Time
	// Subclass is the curator's label on the split series; empty on the total.
	Subclass string
	// ValueUSD is the printed figure as a DECIMAL STRING (ADR-0003).
	ValueUSD string
	// SourceQuery is the curator's public query id the row came from.
	SourceQuery int64
	// ExecutedAt is when the curator's query last ran.
	ExecutedAt time.Time
}

// CuratedRWAPublishedPoint is one (month_end, value) of the total series.
type CuratedRWAPublishedPoint struct {
	MonthEnd time.Time
	ValueUSD string
}

// CuratedRWAPublishedSplit is one (subclass, value) of the latest month's split.
type CuratedRWAPublishedSplit struct {
	Subclass string
	ValueUSD string
}

// CuratedRWAPublished is what the curator last published, read whole.
type CuratedRWAPublished struct {
	// MonthEnd and TotalUSD are the latest point of the total series.
	MonthEnd time.Time
	TotalUSD string
	// SourceQuery and ExecutedAt are the total's provenance; the Split pair is the split's, a
	// separate execution (zero when the split has no rows for MonthEnd).
	SourceQuery      int64
	SplitSourceQuery int64
	ExecutedAt       time.Time
	SplitExecutedAt  time.Time
	// ObservedAt is when this index read the total series.
	ObservedAt time.Time
	// BySubclass is the split for MonthEnd, largest first; empty when that month has no split rows.
	BySubclass []CuratedRWAPublishedSplit
	// Series is the whole total series, oldest first.
	Series []CuratedRWAPublishedPoint
}

// curatedRWAPublishedRecognisedSQL is the recognition bound: rows from a stopped run age out on OUR
// clock after the directory's 48 hours (one missed daily run survivable, two not).
const curatedRWAPublishedRecognisedSQL = `observed_at > now() - INTERVAL '` + curatedRWARecognitionMaxAge + `'`

// curatedRWAPublishedInsertChunk bounds the multi-row insert: 500 rows × 7 params, far under the
// 65535 bind cap.
const curatedRWAPublishedInsertChunk = 500

// ReplaceCuratedRWAPublished makes one curator's cache equal `rows` PER SERIES in one transaction, leaving unnamed series alone; an empty set is refused because "publishes nothing" and "fetch failed" look alike.
func (s *Store) ReplaceCuratedRWAPublished(
	ctx context.Context, curator string, rows []CuratedRWAPublishedRow,
) (inserted int64, err error) {
	if curator == "" {
		return 0, errors.New("curated rwa published: curator must be non-empty")
	}
	if len(rows) == 0 {
		return 0, errors.New("curated rwa published: refusing to sync an empty row set (would empty the curator)")
	}
	rows = dedupCuratedRWAPublishedRows(rows)
	series := make([]string, 0, 2)
	seen := make(map[string]struct{}, 2)
	for _, r := range rows {
		if r.Series == "" || r.ValueUSD == "" || r.MonthEnd.IsZero() || r.ExecutedAt.IsZero() {
			return 0, fmt.Errorf("curated rwa published: row %+v is missing a series, month, value or execution time", r)
		}
		if _, ok := seen[r.Series]; !ok {
			seen[r.Series] = struct{}{}
			series = append(series, r.Series)
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("curated rwa published: begin: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	if _, execErr := tx.ExecContext(ctx, `
		DELETE FROM curated_rwa_published_series
		 WHERE curator = $1 AND series = ANY($2::text[])`, curator, series); execErr != nil {
		err = fmt.Errorf("curated rwa published: delete series: %w", execErr)
		return 0, err
	}
	for start := 0; start < len(rows); start += curatedRWAPublishedInsertChunk {
		end := min(start+curatedRWAPublishedInsertChunk, len(rows))
		q, args := buildCuratedRWAPublishedInsert(curator, rows[start:end])
		res, execErr := tx.ExecContext(ctx, q, args...)
		if execErr != nil {
			err = fmt.Errorf("curated rwa published: insert chunk [%d:%d): %w", start, end, execErr)
			return 0, err
		}
		n, _ := res.RowsAffected()
		inserted += n
	}
	if err = tx.Commit(); err != nil {
		return 0, fmt.Errorf("curated rwa published: commit: %w", err)
	}
	return inserted, nil
}

// dedupCuratedRWAPublishedRows keeps the LAST row per (series, month_end, subclass): a result can
// print one bucket twice, and a repeated key in one multi-row INSERT is refused.
func dedupCuratedRWAPublishedRows(rows []CuratedRWAPublishedRow) []CuratedRWAPublishedRow {
	type key struct {
		series, subclass string
		month            time.Time
	}
	idx := make(map[key]int, len(rows))
	out := make([]CuratedRWAPublishedRow, 0, len(rows))
	for _, r := range rows {
		k := key{r.Series, r.Subclass, r.MonthEnd.UTC().Truncate(24 * time.Hour)}
		if i, ok := idx[k]; ok {
			out[i] = r
			continue
		}
		idx[k] = len(out)
		out = append(out, r)
	}
	return out
}

// buildCuratedRWAPublishedInsert renders one multi-row INSERT. Every bind is cast explicitly, and
// observed_at is transaction-stable now() so one run shares the clock the recognition bound reads.
func buildCuratedRWAPublishedInsert(curator string, rows []CuratedRWAPublishedRow) (string, []any) {
	var b strings.Builder
	b.WriteString(`
		INSERT INTO curated_rwa_published_series
		    (curator, series, month_end, subclass, value_usd, source_query, executed_at, observed_at)
		VALUES `)
	args := make([]any, 0, len(rows)*7)
	for i, r := range rows {
		if i > 0 {
			b.WriteString(", ")
		}
		base := i * 7
		fmt.Fprintf(&b, "($%d, $%d, $%d::date, $%d, $%d::numeric, $%d::bigint, $%d::timestamptz, now())",
			base+1, base+2, base+3, base+4, base+5, base+6, base+7)
		args = append(args, curator, r.Series, r.MonthEnd.UTC().Format("2006-01-02"), r.Subclass,
			r.ValueUSD, r.SourceQuery, r.ExecutedAt.UTC())
	}
	return b.String(), args
}

// curatedRWAPublishedTotalSQL serves the whole recognised total series, oldest first.
const curatedRWAPublishedTotalSQL = `
		SELECT month_end, value_usd::text, source_query, executed_at, observed_at
		  FROM curated_rwa_published_series
		 WHERE curator = $1
		   AND series = '` + CuratedRWASeriesMonthlyTotal + `'
		   AND ` + curatedRWAPublishedRecognisedSQL + `
		 ORDER BY month_end ASC`

// curatedRWAPublishedSplitSQL serves one month's split, largest first. ORDER BY names the TABLE column: an unqualified value_usd
// resolves to the text output name and would sort amounts as text ("904795860.00" above "3100000000.00").
const curatedRWAPublishedSplitSQL = `
		SELECT subclass, value_usd::text, source_query, executed_at
		  FROM curated_rwa_published_series
		 WHERE curator = $1
		   AND series = '` + CuratedRWASeriesMonthlyBySubclass + `'
		   AND month_end = $2::date
		   AND ` + curatedRWAPublishedRecognisedSQL + `
		 ORDER BY curated_rwa_published_series.value_usd DESC, subclass ASC`

// LatestCuratedPublished returns what one curator last published, or (nil, nil) when nothing is inside the recognition bound, so the surface can say `unavailable` rather than fail.
func (s *Store) LatestCuratedPublished(ctx context.Context, curator string) (*CuratedRWAPublished, error) {
	if curator == "" {
		return nil, errors.New("curated rwa published: curator must be non-empty")
	}
	rows, err := s.db.QueryContext(ctx, curatedRWAPublishedTotalSQL, curator)
	if err != nil {
		return nil, fmt.Errorf("curated rwa published: total series: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := &CuratedRWAPublished{}
	for rows.Next() {
		var (
			p                    CuratedRWAPublishedPoint
			executedAt, observed time.Time
		)
		if err := rows.Scan(&p.MonthEnd, &p.ValueUSD, &out.SourceQuery, &executedAt, &observed); err != nil {
			return nil, fmt.Errorf("curated rwa published: scan total: %w", err)
		}
		p.MonthEnd = dateUTC(p.MonthEnd)
		out.Series = append(out.Series, p)
		// Ascending order: the last row read is the latest month.
		out.MonthEnd, out.TotalUSD = p.MonthEnd, p.ValueUSD
		out.ExecutedAt, out.ObservedAt = executedAt.UTC(), observed.UTC()
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("curated rwa published: total rows: %w", err)
	}
	if len(out.Series) == 0 {
		return nil, nil
	}

	split, err := s.db.QueryContext(ctx, curatedRWAPublishedSplitSQL, curator, out.MonthEnd.Format("2006-01-02"))
	if err != nil {
		return nil, fmt.Errorf("curated rwa published: split: %w", err)
	}
	defer func() { _ = split.Close() }()
	for split.Next() {
		var (
			sp         CuratedRWAPublishedSplit
			executedAt time.Time
		)
		if err := split.Scan(&sp.Subclass, &sp.ValueUSD, &out.SplitSourceQuery, &executedAt); err != nil {
			return nil, fmt.Errorf("curated rwa published: scan split: %w", err)
		}
		out.BySubclass = append(out.BySubclass, sp)
		// The split is as fresh as its oldest row.
		if out.SplitExecutedAt.IsZero() || executedAt.Before(out.SplitExecutedAt) {
			out.SplitExecutedAt = executedAt.UTC()
		}
	}
	if err := split.Err(); err != nil {
		return nil, fmt.Errorf("curated rwa published: split rows: %w", err)
	}
	return out, nil
}

// dateUTC pins a scanned `date` to midnight UTC: pgx decodes it in the local zone, which could
// render the previous evening.
func dateUTC(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}
