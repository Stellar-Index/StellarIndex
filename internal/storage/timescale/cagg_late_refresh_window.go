// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"fmt"
	"time"
)

// CAGGLateFamilyTrades is the cagg_late_refresh_windows family of the
// [TradesCAGGs].
const CAGGLateFamilyTrades = "trades"

// CAGGLateRefreshWindow is one view's persisted late-write window
// (migration 0211).
type CAGGLateRefreshWindow struct {
	View      string
	From, To  time.Time
	FirstSeen time.Time
	// Gen changes on every widen; pass it back to
	// [Store.ClearCAGGLateRefreshWindow].
	Gen int64
}

// RecordCAGGLateRefreshWindow records, or widens, [from, to] on each of
// views in family. Every row it touches gets a new gen. Call it before the
// write it covers commits.
func (s *Store) RecordCAGGLateRefreshWindow(ctx context.Context, family string, views []string, from, to time.Time) error {
	const q = `
        INSERT INTO cagg_late_refresh_windows AS w (family, view, from_ts, to_ts)
        SELECT $1, v, $3, $4 FROM unnest($2::text[]) AS v
        ON CONFLICT (family, view) DO UPDATE SET
            from_ts = LEAST(w.from_ts, EXCLUDED.from_ts),
            to_ts   = GREATEST(w.to_ts, EXCLUDED.to_ts),
            gen     = EXCLUDED.gen`
	if _, err := s.db.ExecContext(ctx, q, family, views, from.UTC(), to.UTC()); err != nil {
		return fmt.Errorf("timescale: RecordCAGGLateRefreshWindow (%s): %w", family, err)
	}
	return nil
}

// CAGGLateRefreshWindows returns family's pending windows.
func (s *Store) CAGGLateRefreshWindows(ctx context.Context, family string) ([]CAGGLateRefreshWindow, error) {
	const q = `
        SELECT view, from_ts, to_ts, first_seen, gen
          FROM cagg_late_refresh_windows
         WHERE family = $1
         ORDER BY view`
	rows, err := s.db.QueryContext(ctx, q, family)
	if err != nil {
		return nil, fmt.Errorf("timescale: CAGGLateRefreshWindows (%s): %w", family, err)
	}
	defer func() { _ = rows.Close() }()
	var out []CAGGLateRefreshWindow
	for rows.Next() {
		var w CAGGLateRefreshWindow
		if err := rows.Scan(&w.View, &w.From, &w.To, &w.FirstSeen, &w.Gen); err != nil {
			return nil, fmt.Errorf("timescale: CAGGLateRefreshWindows (%s) scan: %w", family, err)
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: CAGGLateRefreshWindows (%s) rows: %w", family, err)
	}
	return out, nil
}

// ClearCAGGLateRefreshWindow deletes view's window only if its gen is
// still gen, so a widen since it was read survives. deleted reports
// whether a row went.
func (s *Store) ClearCAGGLateRefreshWindow(ctx context.Context, family, view string, gen int64) (deleted bool, err error) {
	res, err := s.db.ExecContext(ctx, clearCAGGLateRefreshWindowQuery, family, view, gen)
	if err != nil {
		return false, fmt.Errorf("timescale: ClearCAGGLateRefreshWindow (%s/%s): %w", family, view, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("timescale: ClearCAGGLateRefreshWindow (%s/%s): %w", family, view, err)
	}
	return n > 0, nil
}

const clearCAGGLateRefreshWindowQuery = `
        DELETE FROM cagg_late_refresh_windows
         WHERE family = $1 AND view = $2 AND gen = $3`
