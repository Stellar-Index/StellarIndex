package timescale

import (
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/freeze"
)

// ─── openLadderRow.entries beside a stale window_ladders map ───
//
// No database: entries() is the pure rule that decides what a row MEANS.
// The executing, real-Postgres half is
// test/integration/freeze_window_ladders_rollback_test.go.

func TestOpenLadderRowEntries_StaleMapFailsClosed(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	fired := now.Add(-100 * time.Minute)
	climbing := freeze.State{FiredAt: fired, HoldUntil: now.Add(4 * time.Minute), ExtensionsUsed: 4, UnfreezeStreak: 2}
	escalated1h := freeze.State{FiredAt: fired, HoldUntil: now.Add(10 * time.Minute), ExtensionsUsed: 4, Escalated: true}

	cases := []struct {
		name        string
		row         openLadderRow
		wantUnowned bool
		want1h      freeze.State
	}{
		{
			// The new binary's own row: the columns ARE the summary.
			name: "in step: map answers verbatim, nothing ownerless",
			row: openLadderRow{
				pair:    freeze.State{FiredAt: fired, HoldUntil: climbing.HoldUntil, ExtensionsUsed: 4},
				windows: map[string]freeze.State{"3600": climbing},
			},
			want1h: climbing,
		},
		{
			// The previous binary escalated; the map still says last rung.
			name: "columns escalated past the map",
			row: openLadderRow{
				pair:    freeze.State{FiredAt: fired, HoldUntil: now.Add(30 * time.Minute), ExtensionsUsed: 4, Escalated: true},
				windows: map[string]freeze.State{"3600": climbing},
			},
			wantUnowned: true,
			want1h:      freeze.State{FiredAt: fired, HoldUntil: now.Add(30 * time.Minute), ExtensionsUsed: 4, Escalated: true},
		},
		{
			// The previous binary's 5m window overwrote the columns with a
			// fresh, LATER hold. Ahead on hold, behind on escalation: the
			// map's escalation must survive the fold.
			name: "columns later but not escalated: the map's escalation survives",
			row: openLadderRow{
				pair:    freeze.State{FiredAt: fired, HoldUntil: now.Add(40 * time.Minute)},
				windows: map[string]freeze.State{"3600": escalated1h},
			},
			wantUnowned: true,
			want1h:      freeze.State{FiredAt: fired, HoldUntil: now.Add(40 * time.Minute), ExtensionsUsed: 4, Escalated: true},
		},
		{
			name: "columns a rung higher than the map",
			row: openLadderRow{
				pair:    freeze.State{FiredAt: fired, HoldUntil: climbing.HoldUntil, ExtensionsUsed: 5},
				windows: map[string]freeze.State{"3600": climbing},
			},
			wantUnowned: true,
			want1h:      freeze.State{FiredAt: fired, HoldUntil: climbing.HoldUntil, ExtensionsUsed: 5},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.row.entries()
			if _, unowned := got[unownedLadderKey]; unowned != tc.wantUnowned {
				t.Errorf("ownerless entry present = %v, want %v (entries %+v)", unowned, tc.wantUnowned, got)
			}
			if tc.wantUnowned && got[unownedLadderKey] != tc.row.pair {
				t.Errorf("ownerless entry = %+v, want the pair-level ladder %+v", got[unownedLadderKey], tc.row.pair)
			}
			if got["3600"] != tc.want1h {
				t.Errorf("1h entry = %+v, want %+v", got["3600"], tc.want1h)
			}
		})
	}
}

// TestPairLadderAhead_IgnoresStoragePrecision: hold_until is timestamptz —
// whole microseconds — while a window_ladders entry keeps the nanoseconds
// time.Now gave it, so the column and the entry it summarises are never
// bit-equal. Measured against the production driver the column is
// TRUNCATED, i.e. never later; this pins that the rule does not depend on
// that, because a driver or parameter format that ROUNDED would otherwise
// read every freeze as stale and rehydrate an ownerless ladder onto windows
// that were never frozen. A real disagreement is minutes, not nanoseconds.
func TestPairLadderAhead_IgnoresStoragePrecision(t *testing.T) {
	entry := time.Date(2026, 9, 18, 12, 25, 0, 1600, time.UTC) // …00.0000016
	summary := freeze.State{FiredAt: entry.Add(-time.Hour), HoldUntil: entry, ExtensionsUsed: 2}
	pair := summary

	pair.HoldUntil = entry.Round(time.Microsecond) // 400ns later: a rounding store
	if pairLadderAhead(pair, summary) {
		t.Error("a column rounded up to the microsecond read as ahead of the entry it summarises")
	}
	pair.HoldUntil = entry.Truncate(time.Microsecond) // 600ns earlier: what pgx stores
	if pairLadderAhead(pair, summary) {
		t.Error("a column truncated to the microsecond read as ahead of the entry it summarises")
	}
	pair.HoldUntil = entry.Add(time.Minute)
	if !pairLadderAhead(pair, summary) {
		t.Error("a hold a minute past the map's did not read as ahead")
	}
}
