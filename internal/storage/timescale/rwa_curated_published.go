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

// curated_rwa_published_series — the totals a third-party curator
// PUBLISHES about tokenized real-world assets on Stellar (migration
// 0162), cached for the RWA surface's curated arm. Synced by
// `stellarindex-ops curated-rwa-sync`.
//
// WHY THIS TABLE, and not rwa_curated_directory alone. The directory
// (0161) was built for the curator's per-asset list and prices. Those
// live in CSV uploads that are PRIVATE to the uploading team — Dune
// refuses them to every outside account — so no sync can fill it. What
// the curator does let anyone read is the latest RESULT of its public
// dashboard queries: a monthly RWA market-cap total and that total split
// by the curator's own subclass labels. This table holds exactly those.
//
// WHAT A ROW MEANS, precisely. "Curator X's public query Q, when it last
// ran at executed_at, printed value_usd for series S at month_end (and
// subclass)." The curator's arithmetic over the curator's private inputs:
// no signature, no per-asset breakdown, no price, no market. Served under
// its own name beside the verified set, never inside it.
//
// TWO CLOCKS. `executed_at` is the CURATOR's — when its query last ran;
// the figure is as fresh as that. `observed_at` is OURS — when this index
// read the result; the reader's recognition bound ages on it, spliced
// into the SQL so no caller can read a stale row as fresh. One sync run
// replaces a curator's rows PER SERIES in one transaction, so every row
// of a series shares one observed_at and one executed_at.

// Series names, shared by the writer and the reader.
const (
	// CuratedRWASeriesMonthlyTotal is one row per month_end, subclass
	// '': the curator's headline monthly RWA market cap.
	CuratedRWASeriesMonthlyTotal = "rwa_mcap_monthly"
	// CuratedRWASeriesMonthlyBySubclass is one row per (month_end,
	// subclass): the same total split by the curator's own labels.
	CuratedRWASeriesMonthlyBySubclass = "rwa_mcap_monthly_by_subclass"
)

// CuratedRWAPublishedRow is one point of a published series, as the
// curator's query printed it.
type CuratedRWAPublishedRow struct {
	Series string
	// MonthEnd is the last day of the month the curator bucketed the
	// figure into, at midnight UTC. Stored as a date.
	MonthEnd time.Time
	// Subclass is the curator's label on the split series; empty on the
	// total series.
	Subclass string
	// ValueUSD is the printed figure as a DECIMAL STRING (ADR-0003).
	ValueUSD string
	// SourceQuery is the curator's public query id the row came from.
	SourceQuery int64
	// ExecutedAt is when the curator's query last ran.
	ExecutedAt time.Time
}

// CuratedRWAPublishedPoint is one (month_end, value) of the total series
// as served.
type CuratedRWAPublishedPoint struct {
	MonthEnd time.Time
	ValueUSD string
}

// CuratedRWAPublishedSplit is one (subclass, value) of the latest month's
// split as served.
type CuratedRWAPublishedSplit struct {
	Subclass string
	ValueUSD string
}

// CuratedRWAPublished is what the curator last published, read whole:
// the latest month's total, its split, and the full monthly series.
type CuratedRWAPublished struct {
	// MonthEnd and TotalUSD are the latest point of the total series —
	// the curator's headline figure.
	MonthEnd time.Time
	TotalUSD string
	// SourceQuery and ExecutedAt are the total series' provenance;
	// SplitSourceQuery is the split's (0 when the split has no rows for
	// MonthEnd).
	SourceQuery      int64
	SplitSourceQuery int64
	ExecutedAt       time.Time
	// ObservedAt is when this index read the total series.
	ObservedAt time.Time
	// BySubclass is the split for MonthEnd, largest first. Empty when the
	// split series has no rows for that month.
	BySubclass []CuratedRWAPublishedSplit
	// Series is the whole total series, oldest first; its last point is
	// (MonthEnd, TotalUSD).
	Series []CuratedRWAPublishedPoint
}

// curatedRWAPublishedRecognisedSQL is the recognition bound: a run that
// stopped leaves rows that age out on OUR clock, the same 48 hours the
// directory uses (one missed daily run survivable, two not).
const curatedRWAPublishedRecognisedSQL = `observed_at > now() - INTERVAL '` + curatedRWARecognitionMaxAge + `'`

// curatedRWAPublishedInsertChunk bounds the multi-row insert: 500 rows ×
// 7 params = 3500 placeholders, far under Postgres's 65535 bind cap.
const curatedRWAPublishedInsertChunk = 500

// ReplaceCuratedRWAPublished makes the cache for one curator equal to
// `rows`, PER SERIES: every series present in `rows` is deleted and
// re-inserted in one transaction, so a series is always one execution's
// whole output. Series the rows do not name are left alone.
//
// An empty set is refused rather than applied: a run that fetched
// nothing cannot tell "the curator publishes nothing" from "the fetch
// failed", and applying it would empty the curator into silence.
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

// dedupCuratedRWAPublishedRows keeps the LAST row per (series,
// month_end, subclass). A published result can print one bucket twice;
// the multi-row insert would then hit the primary key twice in one
// statement, which Postgres refuses.
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

// buildCuratedRWAPublishedInsert renders one multi-row INSERT for a
// chunk. Every bind is cast explicitly; observed_at is transaction-stable
// now(), so every row of one run shares the clock the recognition bound
// reads.
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

// curatedRWAPublishedTotalSQL serves the whole recognised total series,
// oldest first. The recognition bound is in the statement.
const curatedRWAPublishedTotalSQL = `
		SELECT month_end, value_usd::text, source_query, executed_at, observed_at
		  FROM curated_rwa_published_series
		 WHERE curator = $1
		   AND series = '` + CuratedRWASeriesMonthlyTotal + `'
		   AND ` + curatedRWAPublishedRecognisedSQL + `
		 ORDER BY month_end ASC`

// curatedRWAPublishedSplitSQL serves the split for ONE month, largest
// first, under the same bound.
//
// The ORDER BY names the TABLE column. The select list's `value_usd::text`
// takes the output name `value_usd`, and Postgres resolves an unqualified
// ORDER BY against output names first — which sorted "904795860.00"
// above "3100000000.00" as text until the integration test caught it.
const curatedRWAPublishedSplitSQL = `
		SELECT subclass, value_usd::text, source_query
		  FROM curated_rwa_published_series
		 WHERE curator = $1
		   AND series = '` + CuratedRWASeriesMonthlyBySubclass + `'
		   AND month_end = $2::date
		   AND ` + curatedRWAPublishedRecognisedSQL + `
		 ORDER BY curated_rwa_published_series.value_usd DESC, subclass ASC`

// LatestCuratedPublished returns what one curator last published, or
// (nil, nil) when nothing of that curator's is inside the recognition
// bound — an absence, not an error, so the surface can say
// `unavailable` rather than fail.
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
		var sp CuratedRWAPublishedSplit
		if err := split.Scan(&sp.Subclass, &sp.ValueUSD, &out.SplitSourceQuery); err != nil {
			return nil, fmt.Errorf("curated rwa published: scan split: %w", err)
		}
		out.BySubclass = append(out.BySubclass, sp)
	}
	if err := split.Err(); err != nil {
		return nil, fmt.Errorf("curated rwa published: split rows: %w", err)
	}
	return out, nil
}

// dateUTC pins a scanned `date` to midnight UTC. pgx decodes a date into
// the process's local zone; a month_end that reads back as the previous
// evening would render as the wrong day.
func dateUTC(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}
