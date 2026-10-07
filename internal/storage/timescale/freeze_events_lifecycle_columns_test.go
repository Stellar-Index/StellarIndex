package timescale

import (
	"context"
	"database/sql/driver"
	"testing"
	"time"
)

// TestListFreezeEvents_SurfacesLifecycleColumns pins that
// ListFreezeEvents selects, beyond the recover/detail columns,
// migration 0119's hold_until/extensions_used/escalated/
// corroborated — the only fields that distinguish a 10-minute hold
// from a freeze that has climbed the extension ladder to ESCALATED.
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
