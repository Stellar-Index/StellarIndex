//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/entryhistory"
	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/xdrjson"
)

// The issuer-scoped stream keeps every group an asset row comes from, including a claimable-balance removal whose
// row (empty entry, balance-id key) carries no issuer bytes, and drops the groups of other assets and accounts.
func TestStreamIssuerEntryChanges_SameAssetProjectionFewerRows(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const ledger = uint32(69_160_000)
	ct := time.Date(2025, 6, 3, 0, 0, 0, 0, time.UTC)
	asset := cbsAsset(t, "USDC")
	assetID := xdrjson.AssetID(asset)
	marshal := func(v any) string {
		b64, err := xdr.MarshalBase64(v)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return b64
	}
	cbEntry := func(id [32]byte, a xdr.Asset) string {
		h := xdr.Hash(id)
		return marshal(xdr.LedgerEntry{Data: xdr.LedgerEntryData{
			Type: xdr.LedgerEntryTypeClaimableBalance,
			ClaimableBalance: &xdr.ClaimableBalanceEntry{
				BalanceId: xdr.ClaimableBalanceId{Type: xdr.ClaimableBalanceIdTypeClaimableBalanceIdTypeV0, V0: &h},
				Claimants: []xdr.Claimant{{Type: xdr.ClaimantTypeClaimantTypeV0, V0: &xdr.ClaimantV0{
					Destination: xdr.MustAddress(lcbOther), Predicate: xdr.ClaimPredicate{Type: xdr.ClaimPredicateTypeClaimPredicateUnconditional},
				}}},
				Asset: a, Amount: 20,
			},
		}})
	}
	tl := asset.ToTrustLineAsset()
	tlKey := marshal(xdr.LedgerKey{Type: xdr.LedgerEntryTypeTrustline, TrustLine: &xdr.LedgerKeyTrustLine{AccountId: xdr.MustAddress(lcbOther), Asset: tl}})
	tlEntry := marshal(xdr.LedgerEntry{Data: xdr.LedgerEntryData{Type: xdr.LedgerEntryTypeTrustline, TrustLine: &xdr.TrustLineEntry{
		AccountId: xdr.MustAddress(lcbOther), Asset: tl, Balance: 7, Limit: 100,
	}}})
	acctKey := marshal(xdr.LedgerKey{Type: xdr.LedgerEntryTypeAccount, Account: &xdr.LedgerKeyAccount{AccountId: xdr.MustAddress(lcbOther)}})
	acctEntry := marshal(xdr.LedgerEntry{Data: xdr.LedgerEntryData{Type: xdr.LedgerEntryTypeAccount, Account: &xdr.AccountEntry{AccountId: xdr.MustAddress(lcbOther), Balance: 100}}})
	claimed, native := cbsID(t, 0xA1), cbsID(t, 0xA2)
	row := func(tx string, ci uint32, change, entryType, key, entry string) chstore.LedgerEntryChangeRow {
		return chstore.LedgerEntryChangeRow{
			LedgerSeq: ledger, CloseTime: ct, TxHash: tx, ChangeIndex: ci, IntraLedgerSeq: ci,
			ChangeType: change, EntryType: entryType, KeyXDR: key, EntryXDR: entry,
		}
	}
	if _, err := chstore.InsertEntryChanges(ctx, addr, []chstore.LedgerEntryChangeRow{
		row("aes1", 1, "state", "claimable_balance", cbsKeyXDR(t, claimed), cbEntry(claimed, asset)),
		row("aes1", 2, "removed", "claimable_balance", cbsKeyXDR(t, claimed), ""),
		row("aes2", 3, "created", "trustline", tlKey, tlEntry),
		row("aes3", 4, "created", "claimable_balance", cbsKeyXDR(t, native), cbEntry(native, xdr.MustNewNativeAsset())),
		row("aes4", 5, "state", "account", acctKey, acctEntry),
		row("aes4", 6, "updated", "account", acctKey, acctEntry),
	}, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}
	er, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("NewExplorerReader: %v", err)
	}
	raw, err := strkey.Decode(strkey.VersionByteAccountID, cbsIssuer)
	if err != nil {
		t.Fatalf("decode issuer: %v", err)
	}
	project := func(stream func(func(chstore.EntryHistorySourceRow) error) error) (rows int, got []string) {
		p := entryhistory.NewProjector()
		if err := stream(func(s chstore.EntryHistorySourceRow) error {
			rows++
			_, assets, err := p.Project(s)
			for _, a := range assets {
				if a.Asset == assetID {
					got = append(got, fmt.Sprintf("%s/%d %s %s %s", a.TxHash, a.ChangeIndex, a.ChangeType, a.Role, a.Balance))
				}
			}
			return err
		}); err != nil {
			t.Fatalf("stream: %v", err)
		}
		return rows, got
	}
	allRows, want := project(func(fn func(chstore.EntryHistorySourceRow) error) error {
		return er.StreamEntryChangesForLedgers(ctx, []uint32{ledger}, fn)
	})
	rows, got := project(func(fn func(chstore.EntryHistorySourceRow) error) error {
		return er.StreamAccountEntryChangesForLedgers(ctx, string(raw), []uint32{ledger}, fn)
	})
	if !slices.Equal(got, want) || len(want) != 2 {
		t.Errorf("issuer-scoped projection = %v, want %v (the claimable removal and the trustline)", got, want)
	}
	if allRows != 6 || rows != 3 {
		t.Errorf("rows streamed = %d of %d, want 3 of 6 (the native balance and the account dropped)", rows, allRows)
	}
}
