//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// TestExplorerScanQueries_ExecuteAgainstServer proves, against a REAL
// ClickHouse server, that every scan-shaped explorer query refactored in
// route-sweep 2026-07-29 (extracted to builders + pinned with
// `SETTINGS max_threads/max_memory_usage`) still parses and executes — the
// unit tests pin the SQL text; this pins that the text is valid ClickHouse
// (a misplaced SETTINGS clause or a drifted placeholder count fails HERE,
// not in production). Result contents are asserted only where a seeded row
// exercises a code path that would otherwise short-circuit before its
// query (AccountState's trustline/offer reads run only for an EXISTING
// account).
func TestExplorerScanQueries_ExecuteAgainstServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	// A real, checksum-valid account with a real AccountEntry in the
	// current-state projection, so AccountState proceeds past the entry
	// lookup into the pinned trustline + offer scans.
	var seed [32]byte
	seed[0] = 0xE5
	account, err := strkey.Encode(strkey.VersionByteAccountID, seed[:])
	if err != nil {
		t.Fatalf("encode account strkey: %v", err)
	}
	var aid xdr.AccountId
	if err := aid.SetAddress(account); err != nil {
		t.Fatalf("set address: %v", err)
	}
	keyB64, err := xdr.MarshalBase64(xdr.LedgerKey{
		Type:    xdr.LedgerEntryTypeAccount,
		Account: &xdr.LedgerKeyAccount{AccountId: aid},
	})
	if err != nil {
		t.Fatalf("marshal account key: %v", err)
	}
	entryB64, err := xdr.MarshalBase64(xdr.LedgerEntry{
		LastModifiedLedgerSeq: 71_000_001,
		Data: xdr.LedgerEntryData{
			Type: xdr.LedgerEntryTypeAccount,
			Account: &xdr.AccountEntry{
				AccountId:  aid,
				Balance:    5_000_000,
				SeqNum:     7,
				Thresholds: xdr.Thresholds{1, 0, 0, 0},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal account entry: %v", err)
	}
	closeTime := time.Date(2024, 2, 2, 0, 0, 0, 0, time.UTC)
	rows := []chstore.LedgerEntryChangeRow{
		{
			LedgerSeq: 71_000_001, CloseTime: closeTime, TxHash: "e5a1", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 1, ChangeType: "created", EntryType: "account",
			KeyXDR: keyB64, EntryXDR: entryB64, AccountID: account, Balance: 5_000_000,
		},
		{
			LedgerSeq: 71_000_001, CloseTime: closeTime, TxHash: "e5a1", OpIndex: 1, ChangeIndex: 0,
			IntraLedgerSeq: 2, ChangeType: "created", EntryType: "trustline",
			KeyXDR: "e5-trustline-key", EntryXDR: "", AccountID: account,
			Asset: "USDX-" + account, Balance: 42,
		},
	}
	if _, err := chstore.InsertEntryChanges(ctx, addr, rows, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}

	r, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("NewExplorerReader: %v", err)
	}
	defer func() { _ = r.Close() }()

	contract, err := strkey.Encode(strkey.VersionByteContract, seed[:])
	if err != nil {
		t.Fatalf("encode contract strkey: %v", err)
	}
	cursor := chstore.ExplorerCursor{Ledger: 71_000_002, A: 1, B: 1}

	// Every pinned builder executes without a server-side parse/settings
	// error. Empty results are fine — validity, not content, is under test.
	for name, call := range map[string]func() error{
		"RecentOperations(first page)": func() error {
			_, err := r.RecentOperations(ctx, 5, chstore.ExplorerCursor{})
			return err
		},
		"RecentOperations(cursor)": func() error {
			_, err := r.RecentOperations(ctx, 5, cursor)
			return err
		},
		"OperationTypeStats": func() error {
			_, err := r.OperationTypeStats(ctx, 0)
			return err
		},
		"AccountTransactions(first page)": func() error {
			_, err := r.AccountTransactions(ctx, account, 5, chstore.ExplorerCursor{})
			return err
		},
		"AccountTransactions(cursor)": func() error {
			_, err := r.AccountTransactions(ctx, account, 5, cursor)
			return err
		},
		"AccountOperations(first page)": func() error {
			_, err := r.AccountOperations(ctx, account, 5, chstore.ExplorerCursor{})
			return err
		},
		"AccountOperations(cursor)": func() error {
			_, err := r.AccountOperations(ctx, account, 5, cursor)
			return err
		},
		"ContractEventsRecent(first page)": func() error {
			_, err := r.ContractEventsRecent(ctx, contract, 5, chstore.ExplorerCursor{})
			return err
		},
		"ContractEventsRecent(cursor)": func() error {
			_, err := r.ContractEventsRecent(ctx, contract, 5, cursor)
			return err
		},
		"RecentContracts": func() error {
			_, err := r.RecentContracts(ctx, 5, 0)
			return err
		},
		"ContractInteractions": func() error {
			_, err := r.ContractInteractions(ctx, contract, 5, 0)
			return err
		},
		"ContractCodeHistory": func() error {
			_, err := r.ContractCodeHistory(ctx, contract)
			return err
		},
		"AssetHolders": func() error {
			_, _, err := r.AssetHolders(ctx, "USDX-"+account, 5)
			return err
		},
		"AccountsByWealth": func() error {
			_, err := r.AccountsByWealth(ctx, []string{"native"}, []float64{0.4}, 5)
			return err
		},
		"AccountsUnspendable": func() error {
			_, err := r.AccountsUnspendable(ctx, []string{account})
			return err
		},
		"AccountMovements(filter+cursor)": func() error {
			_, err := r.AccountMovements(ctx, account, 5,
				chstore.AccountMovementCursor{Ledger: 71_000_002, TxHash: "ff"},
				chstore.AccountMovementFilter{Kind: "payment", Direction: chstore.AccountMovementSent, Asset: "native"})
			return err
		},
	} {
		if err := call(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}

	// The seeded account exercises the pinned trustline/offer scans through
	// AccountState and must resolve with its real balance + trustline.
	st, err := r.AccountState(ctx, account)
	if err != nil {
		t.Fatalf("AccountState: %v", err)
	}
	if !st.Exists || st.Balance != 5_000_000 {
		t.Errorf("AccountState exists=%v balance=%d, want the seeded entry (true, 5000000)", st.Exists, st.Balance)
	}
	if len(st.Trustlines) != 1 || st.Trustlines[0].Balance != 42 {
		t.Errorf("AccountState trustlines = %+v, want the seeded 42-balance trustline", st.Trustlines)
	}

	// The seeded trustline also proves the holders board end to end.
	holders, total, err := r.AssetHolders(ctx, "USDX-"+account, 5)
	if err != nil || total != 1 || len(holders) != 1 || holders[0].Balance != 42 {
		t.Errorf("AssetHolders = %v total=%d err=%v, want the one seeded holder", holders, total, err)
	}
}
