package clickhouse

import (
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/ingest"
	"github.com/stellar/go-stellar-sdk/xdr"
)

const ecTestG = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

func TestEntryChangeRow_CreatedAccount(t *testing.T) {
	entry := xdr.LedgerEntry{
		LastModifiedLedgerSeq: 100,
		Data: xdr.LedgerEntryData{
			Type: xdr.LedgerEntryTypeAccount,
			Account: &xdr.AccountEntry{
				AccountId: xdr.MustAddress(ecTestG),
				Balance:   1000,
			},
		},
	}
	c := xdr.LedgerEntryChange{Type: xdr.LedgerEntryChangeTypeLedgerEntryCreated, Created: &entry}

	row, ok := entryChangeRow(100, time.Unix(0, 0).UTC(), "txabc", 0, 0, c)
	if !ok {
		t.Fatal("entryChangeRow returned ok=false for a valid created-account change")
	}
	if row.ChangeType != "created" || row.EntryType != "account" {
		t.Errorf("change/entry type = %q/%q, want created/account", row.ChangeType, row.EntryType)
	}
	if row.KeyXDR == "" || row.EntryXDR == "" {
		t.Errorf("key/entry XDR empty: key=%q entry=%q", row.KeyXDR, row.EntryXDR)
	}
	if row.OpIndex != 0 || row.TxHash != "txabc" {
		t.Errorf("row identity = %+v", row)
	}
}

func TestEntryChangeRow_RemovedTrustline(t *testing.T) {
	key := xdr.LedgerKey{
		Type: xdr.LedgerEntryTypeTrustline,
		TrustLine: &xdr.LedgerKeyTrustLine{
			AccountId: xdr.MustAddress(ecTestG),
			Asset:     xdr.MustNewCreditAsset("USDC", ecTestG).ToTrustLineAsset(),
		},
	}
	c := xdr.LedgerEntryChange{Type: xdr.LedgerEntryChangeTypeLedgerEntryRemoved, Removed: &key}

	row, ok := entryChangeRow(100, time.Unix(0, 0).UTC(), "txdef", -1, 5, c)
	if !ok {
		t.Fatal("entryChangeRow returned ok=false for a valid removed-trustline change")
	}
	if row.ChangeType != "removed" || row.EntryType != "trustline" {
		t.Errorf("change/entry type = %q/%q, want removed/trustline", row.ChangeType, row.EntryType)
	}
	if row.KeyXDR == "" {
		t.Error("removed change should still carry the key XDR")
	}
	if row.EntryXDR != "" {
		t.Errorf("removed change should have no entry XDR, got %q", row.EntryXDR)
	}
	if row.OpIndex != -1 { // tx-level / fee-meta marker
		t.Errorf("op_index = %d, want -1", row.OpIndex)
	}
}

// ecTestIssuer is a distinct G-strkey used as the asset issuer so the
// asset-key assertion can't accidentally pass by matching the holder.
const ecTestIssuer = "GBUKBCG5VLRKAVYAIREJRUJHOKLIADZJOICRW43WVJCLES52BDOTCQZU"

func TestOwnerAndAsset_AccountOwnedEntries(t *testing.T) {
	// Account entry → owner set, no asset.
	acct := xdr.LedgerEntry{Data: xdr.LedgerEntryData{
		Type:    xdr.LedgerEntryTypeAccount,
		Account: &xdr.AccountEntry{AccountId: xdr.MustAddress(ecTestG), Balance: 1000},
	}}
	row, ok := entryChangeRow(100, time.Unix(0, 0).UTC(), "tx", 0, 0,
		xdr.LedgerEntryChange{Type: xdr.LedgerEntryChangeTypeLedgerEntryCreated, Created: &acct})
	if !ok || row.AccountID != ecTestG || row.Asset != "" || row.Balance != 1000 {
		t.Errorf("account: account_id=%q asset=%q balance=%d (ok=%v), want %q / \"\" / 1000", row.AccountID, row.Asset, row.Balance, ok, ecTestG)
	}

	// Created trustline → balance carried from the entry.
	tlEntry := xdr.LedgerEntry{Data: xdr.LedgerEntryData{
		Type: xdr.LedgerEntryTypeTrustline,
		TrustLine: &xdr.TrustLineEntry{
			AccountId: xdr.MustAddress(ecTestG),
			Asset:     xdr.MustNewCreditAsset("USDC", ecTestIssuer).ToTrustLineAsset(),
			Balance:   250,
		},
	}}
	row, ok = entryChangeRow(100, time.Unix(0, 0).UTC(), "tx", 0, 9,
		xdr.LedgerEntryChange{Type: xdr.LedgerEntryChangeTypeLedgerEntryCreated, Created: &tlEntry})
	if !ok || row.AccountID != ecTestG || row.Asset != "USDC-"+ecTestIssuer || row.Balance != 250 {
		t.Errorf("created trustline: account_id=%q asset=%q balance=%d, want %q / %q / 250", row.AccountID, row.Asset, row.Balance, ecTestG, "USDC-"+ecTestIssuer)
	}

	// Trustline (removed → key only) → owner = holder, asset = CODE-ISSUER.
	tlKey := xdr.LedgerKey{Type: xdr.LedgerEntryTypeTrustline, TrustLine: &xdr.LedgerKeyTrustLine{
		AccountId: xdr.MustAddress(ecTestG),
		Asset:     xdr.MustNewCreditAsset("USDC", ecTestIssuer).ToTrustLineAsset(),
	}}
	row, ok = entryChangeRow(100, time.Unix(0, 0).UTC(), "tx", 0, 1,
		xdr.LedgerEntryChange{Type: xdr.LedgerEntryChangeTypeLedgerEntryRemoved, Removed: &tlKey})
	wantAsset := "USDC-" + ecTestIssuer
	if !ok || row.AccountID != ecTestG || row.Asset != wantAsset {
		t.Errorf("trustline: account_id=%q asset=%q, want %q / %q", row.AccountID, row.Asset, ecTestG, wantAsset)
	}

	// Offer → owner = seller, no asset.
	offerKey := xdr.LedgerKey{Type: xdr.LedgerEntryTypeOffer, Offer: &xdr.LedgerKeyOffer{
		SellerId: xdr.MustAddress(ecTestG), OfferId: 42,
	}}
	row, ok = entryChangeRow(100, time.Unix(0, 0).UTC(), "tx", 0, 2,
		xdr.LedgerEntryChange{Type: xdr.LedgerEntryChangeTypeLedgerEntryRemoved, Removed: &offerKey})
	if !ok || row.AccountID != ecTestG || row.Asset != "" {
		t.Errorf("offer: account_id=%q asset=%q, want %q / \"\"", row.AccountID, row.Asset, ecTestG)
	}

	// Data entry → owner = account, no asset.
	dataKey := xdr.LedgerKey{Type: xdr.LedgerEntryTypeData, Data: &xdr.LedgerKeyData{
		AccountId: xdr.MustAddress(ecTestG), DataName: "config",
	}}
	row, ok = entryChangeRow(100, time.Unix(0, 0).UTC(), "tx", 0, 3,
		xdr.LedgerEntryChange{Type: xdr.LedgerEntryChangeTypeLedgerEntryRemoved, Removed: &dataKey})
	if !ok || row.AccountID != ecTestG || row.Asset != "" {
		t.Errorf("data: account_id=%q asset=%q, want %q / \"\"", row.AccountID, row.Asset, ecTestG)
	}
}

// TestExtractEntryChanges_IntraLedgerSeqOrdersLastChangeWins pins the
// audit-2026-07-16 C2-4c writer contract: extractEntryChanges stamps a
// per-LEDGER monotonic intra_ledger_seq on every change in canonical walk order
// so that (a) when the SAME key is changed twice in one ledger the LATER change
// carries the HIGHER seq — the tie-breaker ledger_entries_current's
// ReplacingMergeTree version folds in so FINAL keeps the last change — and (b)
// the counter accumulates ACROSS transactions in the ledger (it is NOT the
// per-transaction change_index). Both are required for the composite version
// (ledger_seq<<32 | intra_ledger_seq) to resolve same-ledger ties correctly.
func TestExtractEntryChanges_IntraLedgerSeqOrdersLastChangeWins(t *testing.T) {
	acct := xdr.LedgerEntry{
		LastModifiedLedgerSeq: 100,
		Data: xdr.LedgerEntryData{
			Type:    xdr.LedgerEntryTypeAccount,
			Account: &xdr.AccountEntry{AccountId: xdr.MustAddress(ecTestG), Balance: 500},
		},
	}
	acctKey := xdr.LedgerKey{
		Type:    xdr.LedgerEntryTypeAccount,
		Account: &xdr.LedgerKeyAccount{AccountId: xdr.MustAddress(ecTestG)},
	}
	updated := xdr.LedgerEntryChange{Type: xdr.LedgerEntryChangeTypeLedgerEntryUpdated, Updated: &acct}
	removed := xdr.LedgerEntryChange{Type: xdr.LedgerEntryChangeTypeLedgerEntryRemoved, Removed: &acctKey}

	// A change to an unrelated key, used in a SECOND transaction to prove the
	// counter keeps climbing across txs (a per-tx reset would restart it at 0).
	other := xdr.LedgerEntry{
		LastModifiedLedgerSeq: 100,
		Data: xdr.LedgerEntryData{
			Type: xdr.LedgerEntryTypeTrustline,
			TrustLine: &xdr.TrustLineEntry{
				AccountId: xdr.MustAddress(ecTestG),
				Asset:     xdr.MustNewCreditAsset("USDC", ecTestIssuer).ToTrustLineAsset(),
				Balance:   250,
			},
		},
	}
	otherChange := xdr.LedgerEntryChange{Type: xdr.LedgerEntryChangeTypeLedgerEntryCreated, Created: &other}

	mkTx := func(marker byte, changes ...xdr.LedgerEntryChange) ingest.LedgerTransaction {
		return ingest.LedgerTransaction{
			Result: xdr.TransactionResultPair{TransactionHash: xdr.Hash{marker}},
			UnsafeMeta: xdr.TransactionMeta{
				V:  3,
				V3: &xdr.TransactionMetaV3{Operations: []xdr.OperationMeta{{Changes: changes}}},
			},
		}
	}

	var ext LedgerExtract
	now := time.Unix(0, 0).UTC()
	// tx1: the same account key is updated, then removed, in one ledger.
	// tx2: an unrelated change, later in the same ledger's apply order.
	// One ledger-wide walk, one per-ledger counter across both txs.
	extractLedgerEntryChanges(&ext, []ingest.LedgerTransaction{
		mkTx(0x01, updated, removed),
		mkTx(0x02, otherChange),
	}, 100, now)

	if len(ext.Changes) != 3 {
		t.Fatalf("expected 3 extracted changes, got %d", len(ext.Changes))
	}
	up, rm, tx2 := ext.Changes[0], ext.Changes[1], ext.Changes[2]

	// The update and the removal are the SAME key (premise of the tie).
	if up.KeyXDR != rm.KeyXDR || up.KeyXDR == "" {
		t.Fatalf("update/remove must share one key: up=%q rm=%q", up.KeyXDR, rm.KeyXDR)
	}
	if up.ChangeType != "updated" || rm.ChangeType != "removed" {
		t.Fatalf("change types = %q/%q, want updated/removed", up.ChangeType, rm.ChangeType)
	}

	// (a) The LATER change (removal) carries the HIGHER intra_ledger_seq, so the
	// composite version keeps it — the deleted entry is NOT resurrected.
	if up.IntraLedgerSeq != 0 {
		t.Errorf("update intra_ledger_seq = %d, want 0 (first change in the ledger)", up.IntraLedgerSeq)
	}
	if rm.IntraLedgerSeq != 1 {
		t.Errorf("remove intra_ledger_seq = %d, want 1 (must exceed the update's 0 so the removal wins FINAL)", rm.IntraLedgerSeq)
	}
	if rm.IntraLedgerSeq <= up.IntraLedgerSeq {
		t.Errorf("remove seq %d must be > update seq %d — otherwise FINAL can resurrect the deleted key", rm.IntraLedgerSeq, up.IntraLedgerSeq)
	}

	// (b) The counter accumulated across the tx boundary — tx2's change is 2,
	// NOT reset to 0. change_index (per-tx) would have reset; intra_ledger_seq
	// (per-ledger) must not.
	if tx2.IntraLedgerSeq != 2 {
		t.Errorf("tx2 change intra_ledger_seq = %d, want 2 (per-ledger counter must not reset per transaction)", tx2.IntraLedgerSeq)
	}
}

func TestEntryTypeName(t *testing.T) {
	cases := map[xdr.LedgerEntryType]string{
		xdr.LedgerEntryTypeAccount:          "account",
		xdr.LedgerEntryTypeTrustline:        "trustline",
		xdr.LedgerEntryTypeOffer:            "offer",
		xdr.LedgerEntryTypeContractData:     "contract_data",
		xdr.LedgerEntryTypeClaimableBalance: "claimable_balance",
	}
	for in, want := range cases {
		if got := entryTypeName(in); got != want {
			t.Errorf("entryTypeName(%v) = %q, want %q", in, got, want)
		}
	}
}

// TestExtractLedgerEntryChanges_FeePhasePrecedesApplyPhase pins C2-032
// (audit-2026-07-23) on the LAKE side, in lockstep with
// dispatcher.TestProcessLedger_FeePhasePrecedesApplyPhase.
//
// stellar-core charges the fee for EVERY transaction in the tx set before
// applying ANY of them, so on chain every fee change precedes every
// apply-phase change. Walking per tx (tx1 fee, tx1 apply, tx2 fee, …) gave
// tx2's FEE-phase row a HIGHER intra_ledger_seq than tx1's APPLY-phase row
// for the same key — and intra_ledger_seq is folded into
// ledger_entries_current's ReplacingMergeTree version, so FINAL would keep
// the fee-phase entry as the ledger-final state of that key.
func TestExtractLedgerEntryChanges_FeePhasePrecedesApplyPhase(t *testing.T) {
	acctAt := func(balance int64) xdr.LedgerEntryChange {
		return xdr.LedgerEntryChange{
			Type: xdr.LedgerEntryChangeTypeLedgerEntryUpdated,
			Updated: &xdr.LedgerEntry{
				LastModifiedLedgerSeq: 100,
				Data: xdr.LedgerEntryData{
					Type:    xdr.LedgerEntryTypeAccount,
					Account: &xdr.AccountEntry{AccountId: xdr.MustAddress(ecTestG), Balance: xdr.Int64(balance)},
				},
			},
		}
	}
	mkTx := func(marker byte, fee, apply []xdr.LedgerEntryChange) ingest.LedgerTransaction {
		return ingest.LedgerTransaction{
			Result:     xdr.TransactionResultPair{TransactionHash: xdr.Hash{marker}},
			FeeChanges: xdr.LedgerEntryChanges(fee),
			UnsafeMeta: xdr.TransactionMeta{
				V:  3,
				V3: &xdr.TransactionMetaV3{Operations: []xdr.OperationMeta{{Changes: apply}}},
			},
		}
	}

	// tx1 fee → 990, tx1 apply → 500 (the ledger-FINAL balance), tx2 fee → 980.
	var ext LedgerExtract
	extractLedgerEntryChanges(&ext, []ingest.LedgerTransaction{
		mkTx(0x01, []xdr.LedgerEntryChange{acctAt(990)}, []xdr.LedgerEntryChange{acctAt(500)}),
		mkTx(0x02, []xdr.LedgerEntryChange{acctAt(980)}, nil),
	}, 100, time.Unix(0, 0).UTC())

	if len(ext.Changes) != 3 {
		t.Fatalf("expected 3 extracted changes, got %d", len(ext.Changes))
	}
	seqOf := map[uint32]uint32{} // change_index+op_index composite is awkward; key by row order
	for i, row := range ext.Changes {
		seqOf[uint32(i)] = row.IntraLedgerSeq
	}
	// Row order must be: tx1 fee, tx2 fee, tx1 apply.
	if ext.Changes[0].OpIndex != -1 || ext.Changes[1].OpIndex != -1 || ext.Changes[2].OpIndex != 0 {
		t.Fatalf("walk order op_index = %d,%d,%d, want -1,-1,0 (both fee phases, then the apply phase)",
			ext.Changes[0].OpIndex, ext.Changes[1].OpIndex, ext.Changes[2].OpIndex)
	}
	if ext.Changes[0].TxHash == ext.Changes[1].TxHash {
		t.Fatalf("rows 0 and 1 must be the fee changes of DIFFERENT txs, both got %q", ext.Changes[0].TxHash)
	}
	if ext.Changes[2].IntraLedgerSeq != 2 {
		t.Errorf("tx1's apply-phase row has intra_ledger_seq %d, want 2 — it must outrank BOTH fee-phase rows "+
			"or FINAL publishes a fee-phase balance as the ledger-final balance",
			ext.Changes[2].IntraLedgerSeq)
	}
	// change_index is per-TRANSACTION and must survive the phase split:
	// tx1's fee row is its change 0, tx1's apply row is its change 1.
	if ext.Changes[0].ChangeIndex != 0 || ext.Changes[2].ChangeIndex != 1 {
		t.Errorf("tx1 change_index = %d (fee) / %d (apply), want 0 / 1 — the per-tx counter must not restart "+
			"between the two ledger-wide phases (re-ingest idempotency under the ReplacingMergeTree)",
			ext.Changes[0].ChangeIndex, ext.Changes[2].ChangeIndex)
	}
	if ext.Changes[1].ChangeIndex != 0 {
		t.Errorf("tx2 fee change_index = %d, want 0", ext.Changes[1].ChangeIndex)
	}
}
