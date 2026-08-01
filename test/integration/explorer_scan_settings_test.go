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
	// The trustline + offer rows carry GENUINE LedgerKey XDR: the reader's
	// trustline/offer scans are PK-prefix range reads (`key_xdr LIKE
	// '<52-char real-XDR prefix>%'`, accountEntryKeyPrefix), so a synthetic
	// placeholder key can never match and would silently skip the very path
	// under test (CI-red 2026-07-30: the old "e5-trustline-key" seed).
	var issuerSeed [32]byte
	issuerSeed[0] = 0xE6
	issuer, err := strkey.Encode(strkey.VersionByteAccountID, issuerSeed[:])
	if err != nil {
		t.Fatalf("encode issuer strkey: %v", err)
	}
	var issuerAID xdr.AccountId
	if err := issuerAID.SetAddress(issuer); err != nil {
		t.Fatalf("set issuer address: %v", err)
	}
	var code4 [4]byte
	copy(code4[:], "USDX")
	tlAsset := xdr.TrustLineAsset{
		Type:      xdr.AssetTypeAssetTypeCreditAlphanum4,
		AlphaNum4: &xdr.AlphaNum4{AssetCode: code4, Issuer: issuerAID},
	}
	assetID := "USDX-" + issuer
	tlKeyB64, err := xdr.MarshalBase64(xdr.LedgerKey{
		Type:      xdr.LedgerEntryTypeTrustline,
		TrustLine: &xdr.LedgerKeyTrustLine{AccountId: aid, Asset: tlAsset},
	})
	if err != nil {
		t.Fatalf("marshal trustline key: %v", err)
	}
	tlEntryB64, err := xdr.MarshalBase64(xdr.LedgerEntry{
		LastModifiedLedgerSeq: 71_000_001,
		Data: xdr.LedgerEntryData{
			Type: xdr.LedgerEntryTypeTrustline,
			TrustLine: &xdr.TrustLineEntry{
				AccountId: aid, Asset: tlAsset,
				Balance: 42, Limit: 1_000_000,
				Flags: xdr.Uint32(xdr.TrustLineFlagsAuthorizedFlag),
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal trustline entry: %v", err)
	}
	const offerID = xdr.Int64(9_001)
	offerKeyB64, err := xdr.MarshalBase64(xdr.LedgerKey{
		Type:  xdr.LedgerEntryTypeOffer,
		Offer: &xdr.LedgerKeyOffer{SellerId: aid, OfferId: offerID},
	})
	if err != nil {
		t.Fatalf("marshal offer key: %v", err)
	}
	offerEntryB64, err := xdr.MarshalBase64(xdr.LedgerEntry{
		LastModifiedLedgerSeq: 71_000_001,
		Data: xdr.LedgerEntryData{
			Type: xdr.LedgerEntryTypeOffer,
			Offer: &xdr.OfferEntry{
				SellerId: aid, OfferId: offerID,
				Selling: xdr.Asset{Type: xdr.AssetTypeAssetTypeNative},
				Buying: xdr.Asset{
					Type:      xdr.AssetTypeAssetTypeCreditAlphanum4,
					AlphaNum4: &xdr.AlphaNum4{AssetCode: code4, Issuer: issuerAID},
				},
				Amount: 1_500, Price: xdr.Price{N: 3, D: 2},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal offer entry: %v", err)
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
			KeyXDR: tlKeyB64, EntryXDR: tlEntryB64, AccountID: account,
			Asset: assetID, Balance: 42,
		},
		{
			LedgerSeq: 71_000_001, CloseTime: closeTime, TxHash: "e5a1", OpIndex: 2, ChangeIndex: 0,
			IntraLedgerSeq: 3, ChangeType: "created", EntryType: "offer",
			KeyXDR: offerKeyB64, EntryXDR: offerEntryB64, AccountID: account,
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
			_, _, err := r.AssetHolders(ctx, assetID, 5)
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
	if len(st.Trustlines) != 1 || st.Trustlines[0].Balance != 42 || st.Trustlines[0].Limit != 1_000_000 {
		t.Errorf("AccountState trustlines = %+v, want the seeded 42-balance / 1000000-limit trustline", st.Trustlines)
	}
	// The offer arm is the same PK-prefix range shape; the seeded offer's
	// real LedgerKey must round-trip through it.
	if len(st.Offers) != 1 || st.Offers[0].OfferID != 9_001 || st.Offers[0].Amount != 1_500 {
		t.Errorf("AccountState offers = %+v, want the seeded offer 9001 (amount 1500)", st.Offers)
	}

	// The seeded trustline also proves the holders board end to end.
	holders, total, err := r.AssetHolders(ctx, assetID, 5)
	if err != nil || total != 1 || len(holders) != 1 || holders[0].Balance != 42 {
		t.Errorf("AssetHolders = %v total=%d err=%v, want the one seeded holder", holders, total, err)
	}
}
