// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// A frozen ledgerstream cursor must not become the verdict tip: the network
// keeps closing ledgers, so a verdict stamped against it would read "complete
// to tip" for a tip the chain left behind.
func TestTipFromLiveCursor_RefusesFrozenCursor(t *testing.T) {
	now := time.Date(2026, 9, 1, 5, 30, 0, 0, time.UTC)
	cur := timescale.Cursor{Source: "ledgerstream", LastLedger: 63_000_000, UpdatedAt: now.Add(-3 * time.Hour)}
	tip, err := tipFromLiveCursor(cur, now)
	if err == nil {
		t.Fatalf("tipFromLiveCursor = %d, nil; want an error for a cursor frozen 3h", tip)
	}
	if !strings.Contains(err.Error(), "frozen at ledger 63000000") || !strings.Contains(err.Error(), "-to") {
		t.Errorf("error must name the frozen ledger and the -to override, got: %v", err)
	}
}

func TestTipFromLiveCursor_AcceptsLiveCursor(t *testing.T) {
	now := time.Date(2026, 9, 1, 5, 30, 0, 0, time.UTC)
	for _, age := range []time.Duration{0, 4 * time.Second, 30 * time.Minute, time.Hour} {
		cur := timescale.Cursor{Source: "ledgerstream", LastLedger: 63_000_000, UpdatedAt: now.Add(-age)}
		tip, err := tipFromLiveCursor(cur, now)
		if err != nil || tip != 63_000_000 {
			t.Errorf("cursor %s old: tipFromLiveCursor = %d, %v; want 63000000, nil", age, tip, err)
		}
	}
}

// The nightly wrapper passes -to (cursor minus a margin); a frozen cursor must
// still be refused on that path so computed_at is not refreshed.
func TestResolveVerdictTip_ExplicitToStillRefusesFrozenCursor(t *testing.T) {
	now := time.Date(2026, 9, 1, 5, 30, 0, 0, time.UTC)
	frozen := timescale.Cursor{Source: "ledgerstream", LastLedger: 63_000_000, UpdatedAt: now.Add(-30 * time.Hour)}
	if tip, err := resolveVerdictTip(62_999_900, frozen, now, false); err == nil {
		t.Fatalf("resolveVerdictTip = %d, nil; want refusal for -to against a cursor frozen 30h", tip)
	}
	if tip, err := resolveVerdictTip(0, frozen, now, true); err == nil {
		t.Fatalf("resolveVerdictTip = %d, nil; override without -to must still refuse", tip)
	}
	if tip, err := resolveVerdictTip(62_999_900, frozen, now, true); err != nil || tip != 62_999_900 {
		t.Errorf("override: resolveVerdictTip = %d, %v; want 62999900, nil", tip, err)
	}
}

func TestResolveVerdictTip_LiveCursor(t *testing.T) {
	now := time.Date(2026, 9, 1, 5, 30, 0, 0, time.UTC)
	live := timescale.Cursor{Source: "ledgerstream", LastLedger: 63_000_000, UpdatedAt: now.Add(-4 * time.Second)}
	for _, tc := range []struct{ to, want uint32 }{{0, 63_000_000}, {62_999_900, 62_999_900}} {
		if tip, err := resolveVerdictTip(tc.to, live, now, false); err != nil || tip != tc.want {
			t.Errorf("to=%d: resolveVerdictTip = %d, %v; want %d, nil", tc.to, tip, err, tc.want)
		}
	}
}

func TestVerdictTipFromCursorRead_MissingCursor(t *testing.T) {
	now := time.Now()
	if tip, err := verdictTipFromCursorRead(70_000_000, timescale.Cursor{}, timescale.ErrNotFound, now, false); err != nil || tip != 70_000_000 {
		t.Errorf("-to with no cursor row: tip=%d err=%v; want 70000000, nil", tip, err)
	}
	if tip, err := verdictTipFromCursorRead(0, timescale.Cursor{}, timescale.ErrNotFound, now, false); err == nil {
		t.Errorf("no -to, no cursor row: tip=%d; want error", tip)
	}
	if tip, err := verdictTipFromCursorRead(70_000_000, timescale.Cursor{}, errors.New("conn reset"), now, false); err == nil {
		t.Errorf("-to with transient read error: tip=%d; want error", tip)
	}
}
