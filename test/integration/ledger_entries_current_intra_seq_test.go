//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"

	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// TestLedgerEntriesCurrent_SameLedgerLastChangeWins is the end-to-end proof of
// audit-2026-07-16 C2-4c against real ClickHouse: when one storage key is
// changed twice within the SAME ledger (here update-then-remove), the
// current-state projection stellar.ledger_entries_current must FINAL-resolve to
// the LAST change (the removal), never resurrect the before-image.
//
// Topology exercised: InsertEntryChanges → stellar.ledger_entry_changes → the
// ledger_entries_current_mv materialized view → stellar.ledger_entries_current
// (ReplacingMergeTree on version = ledger_seq<<32 | intra_ledger_seq) → FINAL.
//
// The two rows are inserted in the ADVERSARIAL order [removed, updated] on
// purpose: with the pre-fix ReplacingMergeTree(ledger_seq) both rows tie on
// version, and ClickHouse keeps the LAST-inserted (the 'updated' before-image)
// — i.e. this exact test goes RED on the pre-fix schema. The composite version
// makes the removal (intra_ledger_seq 6 > 5) win regardless of insert order.
func TestLedgerEntriesCurrent_SameLedgerLastChangeWins(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const (
		key    = "c24c-update-then-remove-key"
		ledger = uint32(70_000_000)
	)
	closeTime := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	// Canonical walk order in the ledger: the update (intra_ledger_seq 5) comes
	// before the removal (intra_ledger_seq 6). Rows are handed to the writer in
	// the REVERSE (adversarial) order so a ledger_seq-only version would keep
	// the wrong one.
	rows := []chstore.LedgerEntryChangeRow{
		{
			LedgerSeq: ledger, CloseTime: closeTime, TxHash: "aa", OpIndex: 1, ChangeIndex: 0,
			IntraLedgerSeq: 6, ChangeType: "removed", EntryType: "account", KeyXDR: key,
		},
		{
			LedgerSeq: ledger, CloseTime: closeTime, TxHash: "aa", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 5, ChangeType: "updated", EntryType: "account", KeyXDR: key,
			EntryXDR: "before-image-should-not-win",
		},
	}
	if _, err := chstore.InsertEntryChanges(ctx, addr, rows, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}

	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{addr},
		Auth: clickhouse.Auth{Database: "stellar"},
	})
	if err != nil {
		t.Fatalf("open clickhouse: %v", err)
	}
	defer func() { _ = conn.Close() }()

	var (
		gotChangeType string
		gotVersion    uint64
		gotSeq        uint32
	)
	const q = `SELECT change_type, version, intra_ledger_seq
		FROM stellar.ledger_entries_current FINAL
		WHERE entry_type = 'account' AND key_xdr = ?`
	if err := conn.QueryRow(ctx, q, key).Scan(&gotChangeType, &gotVersion, &gotSeq); err != nil {
		t.Fatalf("read ledger_entries_current FINAL: %v", err)
	}

	// The last intra-ledger change (the removal, seq 6) must be the survivor.
	if gotChangeType != "removed" {
		t.Errorf("FINAL current-state change_type = %q, want \"removed\" (the deleted entry was RESURRECTED — same-ledger tie resolved to a stale before-image)", gotChangeType)
	}
	if gotSeq != 6 {
		t.Errorf("winning intra_ledger_seq = %d, want 6 (the LAST change in the ledger must win)", gotSeq)
	}
	// version = (ledger_seq << 32) | intra_ledger_seq — the monotonic composite.
	if want := (uint64(ledger) << 32) | 6; gotVersion != want {
		t.Errorf("winning version = %d, want %d (ledger_seq<<32 | intra_ledger_seq)", gotVersion, want)
	}
}
