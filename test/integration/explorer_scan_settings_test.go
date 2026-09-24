//go:build integration

package integration_test

import (
	"context"
	"encoding/hex"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
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
	// The account-history readers refuse over an EMPTY ops_by_source, which
	// would skip their builders; one sourced row keeps them under test.
	raw := dialClickHouse(t, ctx, "stellar")
	if err := raw.Exec(ctx, `INSERT INTO stellar.ops_by_source
		(source_account, ledger_seq, tx_index, op_index) VALUES (?, 71000001, 0, 0)`, account); err != nil {
		t.Fatalf("seed ops_by_source: %v", err)
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
			_, err := r.ContractEventsRecent(ctx, contract, 5, chstore.ContractEventsCursor{})
			return err
		},
		"ContractEventsRecent(cursor)": func() error {
			_, err := r.ContractEventsRecent(ctx, contract, 5, chstore.ContractEventsCursor{
				Ledger: cursor.Ledger, TxHash: "ff", OpIndex: cursor.A, EventIndex: cursor.B,
			})
			return err
		},
		"RecentContracts": func() error {
			_, err := r.RecentContracts(ctx, 5, 0)
			return err
		},
		"ContractInteractions": func() error {
			_, _, err := r.ContractInteractions(ctx, contract, 5, 0)
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

// TestContractCodeHistory_ServesTheSeededTimeline asserts ContractCodeHistory's
// returned VALUES on a real server, down both of its paths: first served by
// the keyed contract_instance_changes index (populated by the shipped MV),
// then — with this contract's index rows deleted while the table stays
// non-empty — by the legacy SETTINGS-pinned scan the index miss must fall
// through to. The seeded timeline carries an instance-storage rewrite that
// keeps the executable (must collapse onto the FIRST ledger that installed
// it) and is inserted out of order (the read must sort by ledger).
func TestContractCodeHistory_ServesTheSeededTimeline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	var cidRaw, keeperRaw xdr.Hash
	copy(cidRaw[:], "t427-code-history-timeline-cid")
	copy(keeperRaw[:], "t427-code-history-keeper-cid")
	cid, keeper := xdr.ContractId(cidRaw), xdr.ContractId(keeperRaw)
	contract, err := strkey.Encode(strkey.VersionByteContract, cidRaw[:])
	if err != nil {
		t.Fatalf("encode contract strkey: %v", err)
	}
	h0, h1, hKeeper := t356Hash(0x70), t356Hash(0x71), t356Hash(0x72)
	instKey, entry0 := t356InstanceKeyAndEntry(t, cid, h0)
	_, entry1 := t356InstanceKeyAndEntry(t, cid, h1)
	keeperKey, keeperEntry := t356InstanceKeyAndEntry(t, keeper, hKeeper)

	const deploy, rewrite, upgrade = uint32(72_427_010), uint32(72_427_020), uint32(72_427_030)
	t1 := time.Date(2025, 4, 1, 0, 0, 10, 0, time.UTC)
	t2, t3 := t1.Add(10*time.Second), t1.Add(20*time.Second)
	rows := []chstore.LedgerEntryChangeRow{
		{
			LedgerSeq: upgrade, CloseTime: t3, TxHash: "t427-upgrade", IntraLedgerSeq: 5,
			ChangeType: "updated", EntryType: "contract_data", KeyXDR: instKey, EntryXDR: entry1,
		},
		{
			LedgerSeq: deploy, CloseTime: t1, TxHash: "t427-deploy", IntraLedgerSeq: 2,
			ChangeType: "created", EntryType: "contract_data", KeyXDR: instKey, EntryXDR: entry0,
		},
		{
			LedgerSeq: rewrite, CloseTime: t2, TxHash: "t427-storage", IntraLedgerSeq: 7,
			ChangeType: "updated", EntryType: "contract_data", KeyXDR: instKey, EntryXDR: entry0,
		},
		{
			LedgerSeq: deploy, CloseTime: t1, TxHash: "t427-keeper", IntraLedgerSeq: 3,
			ChangeType: "created", EntryType: "contract_data", KeyXDR: keeperKey, EntryXDR: keeperEntry,
		},
	}
	if _, err := chstore.InsertEntryChanges(ctx, addr, rows, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}
	want := []chstore.ContractCodeVersion{
		{Ledger: deploy, CloseTime: t1, WasmHash: hex.EncodeToString(h0[:])},
		{Ledger: upgrade, CloseTime: t3, WasmHash: hex.EncodeToString(h1[:])},
	}

	conn := dialClickHouse(t, ctx, "stellar")
	cidHex, keeperHex := hex.EncodeToString(cidRaw[:]), hex.EncodeToString(keeperRaw[:])
	if n := countInstanceIndexRows(t, ctx, conn, cidHex); n != 3 {
		t.Fatalf("contract_instance_changes holds %d rows for the contract, want the 3 seeded writes", n)
	}
	assertCodeHistory(t, ctx, addr, contract, "index-served", want)

	// Index miss for this contract on a non-empty (so "available") index:
	// the reader must not trust the empty indexed answer.
	syncCtx := clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{"mutations_sync": "2"}))
	if err := conn.Exec(syncCtx,
		`ALTER TABLE stellar.contract_instance_changes DELETE WHERE contract_hash = ?`, cidHex); err != nil {
		t.Fatalf("delete index rows: %v", err)
	}
	if n := countInstanceIndexRows(t, ctx, conn, cidHex); n != 0 {
		t.Fatalf("contract_instance_changes still holds %d rows for the contract after the delete", n)
	}
	if n := countInstanceIndexRows(t, ctx, conn, keeperHex); n != 1 {
		t.Fatalf("keeper contract has %d index rows, want 1 (the index must stay non-empty)", n)
	}
	assertCodeHistory(t, ctx, addr, contract, "legacy-scan fallback", want)
}

func countInstanceIndexRows(t *testing.T, ctx context.Context, conn driver.Conn, contractHash string) uint64 {
	t.Helper()
	var n uint64
	if err := conn.QueryRow(ctx, `SELECT count() FROM stellar.contract_instance_changes FINAL
		WHERE contract_hash = ?`, contractHash).Scan(&n); err != nil {
		t.Fatalf("count contract_instance_changes: %v", err)
	}
	return n
}

// assertCodeHistory reads through a FRESH reader so its index-availability
// probe reflects the table as it is now, not a cached earlier verdict.
func assertCodeHistory(t *testing.T, ctx context.Context, addr, contract, leg string, want []chstore.ContractCodeVersion) {
	t.Helper()
	r, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("%s: NewExplorerReader: %v", leg, err)
	}
	defer func() { _ = r.Close() }()
	got, err := r.ContractCodeHistory(ctx, contract)
	if err != nil {
		t.Fatalf("%s: ContractCodeHistory: %v", leg, err)
	}
	if len(got) != len(want) {
		t.Fatalf("%s: code history = %+v, want %+v", leg, got, want)
	}
	for i := range want {
		if got[i].Ledger != want[i].Ledger || got[i].WasmHash != want[i].WasmHash ||
			!got[i].CloseTime.Equal(want[i].CloseTime) {
			t.Fatalf("%s: code history[%d] = %+v, want %+v (full %+v)", leg, i, got[i], want[i], got)
		}
	}
}
