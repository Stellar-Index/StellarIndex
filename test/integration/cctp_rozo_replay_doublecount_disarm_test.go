//go:build integration

package integration_test

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"

	"github.com/Stellar-Index/StellarIndex/internal/sources/cctp"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestCCTPReplayDoubleCount_DisarmedByMigration0164 is the T434 proof.
//
// Migration 0112 added event_index to cctp_events' PK and backfilled
// existing rows to event_index=0 on the premise that a prescribed
// re-ingest needs no DELETE. That premise is false: dispatcher_adapter.go
// stamps EventIndex from the event's TRUE intra-op position (cctp's own
// decoder doc: a deposit_for_burn op typically also emits message_sent in
// the SAME op), so a legacy row stored at event_index=0 pre-0112 and a
// projector-replay row computed at its true nonzero index are two DIFFERENT
// PKs for the SAME real-world event — a double-count, not a recovery.
//
// This test reproduces that shape directly against migration 0112 (up to
// 0163, before the disarm): a legacy event_index=0 row plus a "replay" row
// for the same op at its true nonzero index leaves TWO rows for what
// should be one event. It then applies migration 0164 and asserts the
// disarm clears the duplicated legacy state.
func TestCCTPReplayDoubleCount_DisarmedByMigration0164(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrationsUpTo(t, dsn, 163)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ts := time.Now().UTC().Truncate(time.Second)
	const (
		txHash  = "cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcddc"
		ledger  = uint32(62_146_641)
		opIndex = uint32(0)
	)
	mk := func(idx uint32) timescale.CCTPEvent {
		return timescale.CCTPEvent{
			ContractID: cctp.MainnetMessageTransmitter,
			Ledger:     ledger,
			TxHash:     txHash,
			OpIndex:    opIndex,
			EventIndex: idx,
			ObservedAt: ts,
			EventType:  timescale.CCTPDepositForBurn,
			Attributes: map[string]any{"nonce": "1"},
		}
	}

	// Legacy pre-0112 row: backfilled to event_index=0.
	if err := store.InsertCCTPEvent(ctx, mk(0)); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	// Catch-up replay recomputes the event's TRUE intra-op index (nonzero,
	// because message_sent precedes it in this op) and writes a NEW row —
	// a different PK for the SAME real event.
	if err := store.InsertCCTPEvent(ctx, mk(3)); err != nil {
		t.Fatalf("insert replay row: %v", err)
	}

	got := countRows(t, store, `SELECT COUNT(*) FROM cctp_events WHERE tx_hash = $1 AND op_index = $2 AND event_type = 'deposit_for_burn'`,
		txHash, int(opIndex))
	if got != 2 {
		t.Fatalf("pre-disarm cctp_events rows for one real event = %d, want 2 — the double-count this test reproduces did not occur; T434 may already be fixed upstream", got)
	}

	applyMigrationTo164(t, dsn)

	after := countRows(t, store, `SELECT COUNT(*) FROM cctp_events`)
	if after != 0 {
		t.Fatalf("cctp_events rows after migration 0164 = %d, want 0 — disarm did not clear the doubled legacy rows", after)
	}
}

// applyMigrationTo164 advances a database already at 0163 to 0164 (the
// cctp/rozo replay double-count disarm), mirroring applyMigrationsUpTo but
// on an already-open migrate instance's next step.
func applyMigrationTo164(t *testing.T, dsn string) {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	migrationsDir := filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")
	m, err := migrate.New("file://"+migrationsDir, dsn)
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	defer func() { _, _ = m.Close() }()
	if err := m.Migrate(164); err != nil {
		t.Fatalf("migrate to 164: %v", err)
	}
}
