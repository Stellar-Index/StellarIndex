//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/sources/cctp"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestCCTPRozoEventIndex_SameTypeEventsInOneOpDoNotCollapse is the C2-13a
// proof. Migrations 0038 / 0039 keyed cctp_events / rozo_events on
// (contract_id, ledger, tx_hash, op_index, event_type, ts) — no intra-op
// discriminator. A single operation that emits TWO events of the SAME type
// (observed on mainnet, e.g. two `attester_enabled` in one MessageTransmitter
// tx) produced two rows that shared every PK column, so the second collapsed
// onto the first under ON CONFLICT — silently dropping it.
//
// Migration 0112 adds event_index to the key. This test inserts two
// same-type events that differ ONLY in event_index and asserts BOTH land.
// Before the fix, COUNT is 1 (collapsed); after, it is 2.
func TestCCTPRozoEventIndex_SameTypeEventsInOneOpDoNotCollapse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ts := time.Now().UTC().Truncate(time.Second)
	const (
		txHash  = "ababababababababababababababababababababababababababababababab" // 62 chars; + 2-char suffix below = 64 (char(64))
		ledger  = uint32(62_146_641)
		opIndex = uint32(0)
	)

	t.Run("cctp two same-type events in one op", func(t *testing.T) {
		mk := func(idx uint32) timescale.CCTPEvent {
			return timescale.CCTPEvent{
				ContractID: cctp.MainnetMessageTransmitter,
				Ledger:     ledger,
				TxHash:     txHash + "cc", // pad to 64
				OpIndex:    opIndex,
				EventIndex: idx,
				ObservedAt: ts,
				EventType:  timescale.CCTPAttesterEnabled,
				Attributes: map[string]any{"attester": fmt.Sprintf("att-%d", idx)},
			}
		}
		if err := store.InsertCCTPEvent(ctx, mk(0)); err != nil {
			t.Fatalf("InsertCCTPEvent idx=0: %v", err)
		}
		if err := store.InsertCCTPEvent(ctx, mk(1)); err != nil {
			t.Fatalf("InsertCCTPEvent idx=1: %v", err)
		}
		got := countRows(t, store, `SELECT COUNT(*) FROM cctp_events WHERE contract_id = $1 AND ledger = $2 AND tx_hash = $3 AND op_index = $4 AND event_type = 'attester_enabled'`,
			cctp.MainnetMessageTransmitter, int(ledger), txHash+"cc", int(opIndex))
		if got != 2 {
			t.Fatalf("cctp_events rows = %d, want 2 — two same-type events in one op collapsed (C2-13a regression)", got)
		}
		// The two rows must carry distinct event_index (0 and 1).
		idxSum := countRows(t, store, `SELECT COALESCE(SUM(event_index), -1) FROM cctp_events WHERE contract_id = $1 AND ledger = $2 AND tx_hash = $3 AND op_index = $4 AND event_type = 'attester_enabled'`,
			cctp.MainnetMessageTransmitter, int(ledger), txHash+"cc", int(opIndex))
		if idxSum != 1 { // 0 + 1
			t.Errorf("sum(event_index) = %d, want 1 (indexes 0 and 1)", idxSum)
		}
	})

	t.Run("rozo two same-type events in one op", func(t *testing.T) {
		from := "GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAWHF"
		mk := func(idx uint32) timescale.RozoEvent {
			f := from
			return timescale.RozoEvent{
				ContractID:  "CROZOPAYMENTCONTRACTAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
				Ledger:      ledger,
				TxHash:      txHash + "rz", // pad to 64
				OpIndex:     opIndex,
				EventIndex:  idx,
				ObservedAt:  ts,
				EventType:   timescale.RozoPayment,
				Amount:      "1000000",
				Destination: "GDESTAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
				From:        &f,
			}
		}
		if err := store.InsertRozoEvent(ctx, mk(0)); err != nil {
			t.Fatalf("InsertRozoEvent idx=0: %v", err)
		}
		if err := store.InsertRozoEvent(ctx, mk(1)); err != nil {
			t.Fatalf("InsertRozoEvent idx=1: %v", err)
		}
		got := countRows(t, store, `SELECT COUNT(*) FROM rozo_events WHERE tx_hash = $1 AND op_index = $2 AND event_type = 'payment'`,
			txHash+"rz", int(opIndex))
		if got != 2 {
			t.Fatalf("rozo_events rows = %d, want 2 — two same-type events in one op collapsed (C2-13a regression)", got)
		}
	})
}

// countRows runs a single-integer-returning query and returns the value.
func countRows(t *testing.T, store *timescale.Store, q string, args ...any) int {
	t.Helper()
	var n int
	if err := store.DB().QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("countRows: %v", err)
	}
	return n
}
