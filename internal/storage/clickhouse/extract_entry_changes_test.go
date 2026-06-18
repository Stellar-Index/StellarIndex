package clickhouse

import (
	"testing"
	"time"

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
