package timescale

import (
	"context"
	"database/sql/driver"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/anomaly"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/freeze"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// openLadderRow.entries beside a stale window_ladders map. No database: entries() is the pure rule deciding
// what a row MEANS; the real-Postgres half is test/integration/freeze_test.go.

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

// TestPairLadderAhead_IgnoresStoragePrecision: hold_until is timestamptz (microseconds) while a
// window_ladders entry keeps nanoseconds, so they are never bit-equal. The rule must not depend on driver
// truncation, or a rounding driver would read every freeze as stale. Real disagreement is minutes.
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

// fakeLedgerProvider is a stub LedgerProvider returning a fixed ledger.
type fakeLedgerProvider struct{ ledger uint32 }

func (f fakeLedgerProvider) LatestLedger() uint32 { return f.ledger }

// TestRecordFreeze_StampsLedgerFromProvider pins that frozen_at_ledger reflects a wired LedgerProvider, not
// the 0 sentinel; without WithFreezeLedgerProvider no caller can set getLedger, so every production insert
// would stamp 0.
func TestRecordFreeze_StampsLedgerFromProvider(t *testing.T) {
	// Statement 0: advisory lock exec. Statement 1: the INSERT.
	store, conn := newScriptedStore(t,
		scriptedResult{rowsAffected: 0},
		scriptedResult{rowsAffected: 1},
	)

	sink := NewFreezeEventSink(store, WithFreezeLedgerProvider(fakeLedgerProvider{ledger: 918273}))

	asset, err := canonical.ParseAsset("crypto:BTC")
	if err != nil {
		t.Fatalf("ParseAsset: %v", err)
	}
	quote, err := canonical.ParseAsset("fiat:USD")
	if err != nil {
		t.Fatalf("ParseAsset: %v", err)
	}

	if err := sink.RecordFreeze(context.Background(), asset, quote, "1.23", anomaly.Decision{}); err != nil {
		t.Fatalf("RecordFreeze: %v", err)
	}

	if len(conn.stmts) != 2 {
		t.Fatalf("expected 2 statements, got %d", len(conn.stmts))
	}
	insert := conn.stmts[1]
	got := insert.arg(t, 4)
	if got != int64(918273) {
		t.Errorf("frozen_at_ledger arg = %v, want 918273 (from the wired LedgerProvider, not the 0 sentinel)", got)
	}
}

// TestListFreezeEvents_SurfacesLifecycleColumns pins that ListFreezeEvents selects migration 0119's
// hold_until, extensions_used, escalated and corroborated, the only fields separating a 10-minute hold from
// an ESCALATED freeze.
func TestListFreezeEvents_SurfacesLifecycleColumns(t *testing.T) {
	holdUntil := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	store, conn := newScriptedStore(t, scriptedResult{
		cols: []string{
			"asset_id", "quote_id", "frozen_at", "frozen_at_ledger", "reason",
			"frozen_value", "recovered_at", "recovered_at_ledger", "detail",
			"hold_until", "extensions_used", "escalated", "corroborated",
		},
		rows: [][]driver.Value{
			{
				"crypto:BTC", "fiat:USD", time.Unix(0, 0).UTC(), int64(100), "outlier_storm",
				"1", nil, nil, "",
				holdUntil, int64(2), true, false,
			},
		},
	})

	rows, err := store.ListFreezeEvents(context.Background(), true, 100)
	if err != nil {
		t.Fatalf("ListFreezeEvents: %v", err)
	}
	if len(conn.statements()) != 1 {
		t.Fatalf("expected 1 query, got %d", len(conn.statements()))
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	got := rows[0]
	if got.HoldUntil == nil || !got.HoldUntil.Equal(holdUntil) {
		t.Errorf("HoldUntil = %v, want %v", got.HoldUntil, holdUntil)
	}
	if got.ExtensionsUsed == nil || *got.ExtensionsUsed != 2 {
		t.Errorf("ExtensionsUsed = %v, want 2", got.ExtensionsUsed)
	}
	if got.Escalated == nil || !*got.Escalated {
		t.Errorf("Escalated = %v, want true", got.Escalated)
	}
	if got.Corroborated == nil || *got.Corroborated {
		t.Errorf("Corroborated = %v, want false", got.Corroborated)
	}
}

// A nil db proves the refusal happens before any SQL runs.
func TestMarkRecovered_RejectsMalformedReleasedBy(t *testing.T) {
	sink := &FreezeEventSink{}
	asset := canonical.NativeAsset()
	quote, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatalf("quote: %v", err)
	}
	for _, v := range []string{"", "ash", "operator:", "system: ", "Operator:ash", "admin:ash"} {
		if err := sink.MarkRecovered(context.Background(), asset, quote, v); err == nil {
			t.Errorf("MarkRecovered(released_by=%q) = nil, want a refusal", v)
		}
	}
}

func TestValidReleasedBy_AcceptsBothKinds(t *testing.T) {
	for _, v := range []string{"operator:ash", "system:recovery"} {
		if !validReleasedBy(v) {
			t.Errorf("validReleasedBy(%q) = false, want true", v)
		}
	}
}
