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

const (
	lcbHolder = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	lcbOther  = "GDIEKKIQWMIZ4LD3RP3ABPN7X5KEAEWYMR634BRHB7EULIMEVREWLF3G"
	lcbLedger = uint32(69_100_000)
)

func lcbEntryXDR(t *testing.T, id [32]byte, amount int64, claimants ...string) string {
	t.Helper()
	h := xdr.Hash(id)
	cs := make([]xdr.Claimant, 0, len(claimants))
	for _, c := range claimants {
		cs = append(cs, xdr.Claimant{Type: xdr.ClaimantTypeClaimantTypeV0, V0: &xdr.ClaimantV0{
			Destination: xdr.MustAddress(c), Predicate: xdr.ClaimPredicate{Type: xdr.ClaimPredicateTypeClaimPredicateUnconditional},
		}})
	}
	b64, err := xdr.MarshalBase64(xdr.LedgerEntry{Data: xdr.LedgerEntryData{
		Type: xdr.LedgerEntryTypeClaimableBalance,
		ClaimableBalance: &xdr.ClaimableBalanceEntry{
			BalanceId: xdr.ClaimableBalanceId{Type: xdr.ClaimableBalanceIdTypeClaimableBalanceIdTypeV0, V0: &h},
			Claimants: cs, Asset: xdr.MustNewNativeAsset(), Amount: xdr.Int64(amount),
		},
	}})
	if err != nil {
		t.Fatalf("marshal claimable entry: %v", err)
	}
	return b64
}

// The reader returns only balances still on the ledger: one claimed in a later ledger (state pre-image, then removal)
// and one claimed in its creation ledger both resolve to the removal, and a balance naming only another account is
// not prefiltered in. A balance with change rows but no current-state row is counted as unresolved, never served.
func TestLiveClaimableBalancesInLedgers_OnlyUnclaimedForTheAccount(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	live, claimedLater, claimedSame, foreign, orphan := cbsID(t, 0x51), cbsID(t, 0x52), cbsID(t, 0x53), cbsID(t, 0x54), cbsID(t, 0x55)
	ct := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	row := func(ledger, seq uint32, tx, change string, id [32]byte, entry string) chstore.LedgerEntryChangeRow {
		return chstore.LedgerEntryChangeRow{
			LedgerSeq: ledger, CloseTime: ct, TxHash: tx, IntraLedgerSeq: seq, ChangeType: change,
			EntryType: "claimable_balance", KeyXDR: cbsKeyXDR(t, id), EntryXDR: entry,
		}
	}
	mine := func(id [32]byte, amt int64) string { return lcbEntryXDR(t, id, amt, lcbOther, lcbHolder) }
	// Inserted with each removal BEFORE its live row so insertion order cannot pick the winner.
	if _, err := chstore.InsertEntryChanges(ctx, addr, []chstore.LedgerEntryChangeRow{
		row(lcbLedger+2, 2, "lcb2b", "removed", claimedLater, ""),
		row(lcbLedger+2, 1, "lcb2b", "state", claimedLater, mine(claimedLater, 20)),
		row(lcbLedger, 1, "lcb1", "created", live, mine(live, 10)),
		row(lcbLedger, 2, "lcb2", "created", claimedLater, mine(claimedLater, 20)),
		row(lcbLedger+1, 2, "lcb3", "removed", claimedSame, ""),
		row(lcbLedger+1, 1, "lcb3", "created", claimedSame, mine(claimedSame, 30)),
		row(lcbLedger+1, 3, "lcb4", "created", foreign, lcbEntryXDR(t, foreign, 40, lcbOther)),
		row(lcbLedger, 3, "lcb5", "created", orphan, mine(orphan, 50)),
	}, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}
	conn := dialClickHouse(t, ctx, "stellar")
	if err := conn.Exec(ctx, `ALTER TABLE stellar.ledger_entries_current DELETE
		WHERE entry_type = 'claimable_balance' AND key_xdr = $1 SETTINGS mutations_sync = 2`, cbsKeyXDR(t, orphan)); err != nil {
		t.Fatalf("drop the orphan's current-state row: %v", err)
	}

	er, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("NewExplorerReader: %v", err)
	}
	raw, err := strkey.Decode(strkey.VersionByteAccountID, lcbHolder)
	if err != nil {
		t.Fatalf("decode holder: %v", err)
	}
	scan, err := er.LiveClaimableBalancesInLedgers(ctx, string(raw), []uint32{lcbLedger, lcbLedger + 1, lcbLedger + 2}, 500)
	if err != nil {
		t.Fatalf("LiveClaimableBalancesInLedgers: %v", err)
	}
	if scan.Keys != 4 || scan.Unresolved != 1 {
		t.Errorf("keys/unresolved = %d/%d, want 4/1 (foreign not prefiltered in; the orphan unresolved)", scan.Keys, scan.Unresolved)
	}
	got := scan.Live
	if len(got) != 1 {
		t.Fatalf("live balances = %d, want only the unclaimed one (a claimed balance was resurrected)", len(got))
	}
	wantID := xdr.Hash(live).HexString()
	if g := got[0]; g.Fields["balance_id"] != wantID || g.Fields["amount"] != "10" || g.Ledger != lcbLedger ||
		len(g.Claimants) != 2 || g.Claimants[1] != lcbHolder {
		t.Errorf("live balance = %v claimants %v at %d, want %s amount 10 at %d claimable by the holder", g.Fields, g.Claimants, g.Ledger, wantID, lcbLedger)
	}

	if s, err := er.LiveClaimableBalancesInLedgers(ctx, string(raw), []uint32{lcbLedger, lcbLedger + 1, lcbLedger + 2}, 2); err != nil || s.Keys != 2 {
		t.Errorf("with maxKeys 2: keys = %d, err = %v; want the cap reported as 2", s.Keys, err)
	}
}
