//go:build integration

package integration_test

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"

	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// TestCreatorsRollup_RecycledAddressCountsOnce is the recycled-address proof
// (#541) against real ClickHouse. stellar.account_creators_ops is one row
// per creation OPERATION, so one creator recycling one address (create ->
// merge -> create) produces two rows sharing the same `created`. Pre-fix,
// the board's live_accounts/live_stroops joined the live-entry set onto
// that per-event table directly, so the still-live address was counted
// TWICE and its balance summed TWICE. The fix aggregates over the distinct
// (creator, created) pair before the outer sum.
//
// Fixture: one creator, one address created by two distinct operations,
// live with a single known balance. The served board row must show
// live_accounts = 1 and live_stroops = that one balance, not 2x.
func TestCreatorsRollup_RecycledAddressCountsOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const (
		ledger    = uint32(41)
		txHashOne = "recycledaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa1"
		txHashTwo = "recycledaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa2"
		balance   = int64(100_000_000) // one XLM's worth of stroops, the survivor's true balance
	)
	creator := gAccountFromSeed(t, 0x41)
	created := gAccountFromSeed(t, 0x42)
	closeTime := time.Date(2027, 9, 2, 0, 0, 41, 0, time.UTC)

	if err := chstore.EnsureAccountMovementsTable(ctx, addr); err != nil {
		t.Fatalf("EnsureAccountMovementsTable: %v", err)
	}

	sink, err := chstore.Open(ctx, addr, 1000)
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close(ctx) })
	if err := sink.Add(ctx, chstore.LedgerExtract{
		Ledger: chstore.LedgerRow{
			LedgerSeq: ledger, CloseTime: closeTime,
			LedgerHash: "aa04", PrevHash: "bb04", ProtocolVersion: 23, BucketListHash: "cc04",
			TotalCoins: 1, FeePool: 1, BaseFee: 100, BaseReserve: 5_000_000,
		},
		Ops: []chstore.OperationRow{
			{
				LedgerSeq: ledger, CloseTime: closeTime, TxHash: txHashOne, TxIndex: 0, OpIndex: 0,
				OpType: "OperationTypeCreateAccount", SourceAccount: creator,
			},
			{
				LedgerSeq: ledger, CloseTime: closeTime, TxHash: txHashTwo, TxIndex: 1, OpIndex: 0,
				OpType: "OperationTypeCreateAccount", SourceAccount: creator,
			},
		},
	}); err != nil {
		t.Fatalf("sink add: %v", err)
	}
	if err := sink.Flush(ctx); err != nil {
		t.Fatalf("sink flush: %v", err)
	}

	// Two funding legs for the SAME (creator, created) pair — the recycle:
	// created, presumably merged away, created again by the same funder.
	if _, err := chstore.InsertAccountMovements(ctx, addr, []chstore.AccountMovement{
		{
			MovementKind:    "transfer",
			Provenance:      chstore.ProvenanceCAP67Derived,
			Ledger:          ledger,
			LedgerCloseTime: closeTime,
			TxHash:          txHashOne,
			OpIndex:         0,
			LegIndex:        0,
			Asset:           "native",
			Amount:          big.NewInt(50_000_000),
			FromAddress:     creator,
			ToAddress:       created,
		},
		{
			MovementKind:    "transfer",
			Provenance:      chstore.ProvenanceCAP67Derived,
			Ledger:          ledger,
			LedgerCloseTime: closeTime,
			TxHash:          txHashTwo,
			OpIndex:         0,
			LegIndex:        0,
			Asset:           "native",
			Amount:          big.NewInt(50_000_000),
			FromAddress:     creator,
			ToAddress:       created,
		},
	}); err != nil {
		t.Fatalf("InsertAccountMovements: %v", err)
	}

	// The address's current live state: one account entry, one balance.
	if _, err := chstore.InsertEntryChanges(ctx, addr, []chstore.LedgerEntryChangeRow{
		{
			LedgerSeq: ledger, CloseTime: closeTime, TxHash: txHashTwo, OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 1, ChangeType: "updated", EntryType: "account",
			KeyXDR: "recycled-account-key-" + created, EntryXDR: "recycled-account-entry",
			AccountID: created, Balance: balance,
		},
	}, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}

	if err := chstore.RunCreatorsRollup(ctx, addr, 1, t.Logf); err != nil {
		t.Fatalf("RunCreatorsRollup: %v", err)
	}

	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{addr},
		Auth: clickhouse.Auth{Database: "stellar"},
	})
	if err != nil {
		t.Fatalf("open clickhouse: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	var (
		accountsCreated uint64
		liveAccounts    uint64
		liveStroops     big.Int
	)
	const q = `SELECT accounts_created, live_accounts, live_stroops
		FROM stellar.account_creators_rollup
		WHERE creator = ?`
	if err := conn.QueryRow(ctx, q, creator).Scan(&accountsCreated, &liveAccounts, &liveStroops); err != nil {
		t.Fatalf("read account_creators_rollup: %v", err)
	}

	// accounts_created is immutable history and stays per-event: two
	// creation operations, so two.
	if accountsCreated != 2 {
		t.Errorf("accounts_created = %d, want 2 (immutable per-event history)", accountsCreated)
	}
	// live_accounts/live_stroops describe the SURVIVING set: one address,
	// its one true balance — not doubled by the two creation events that
	// produced it.
	if liveAccounts != 1 {
		t.Errorf("live_accounts = %d, want 1 (one surviving address recycled twice, not 2)", liveAccounts)
	}
	if liveStroops.Cmp(big.NewInt(balance)) != 0 {
		t.Errorf("live_stroops = %s, want %d (the address's one true balance, not summed once per creation event)", liveStroops.String(), balance)
	}
}
