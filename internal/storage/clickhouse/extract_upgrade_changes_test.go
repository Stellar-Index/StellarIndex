package clickhouse_test

import (
	"errors"
	"io"
	"sort"
	"testing"

	"github.com/stellar/go-stellar-sdk/ingest"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// TestExtractLedger_UpgradeChangesMatchLedgerChangeReader: a ledger upgrade that
// shrinks an account's liabilities and deletes an underfunded offer (the
// protocol-10 prepareLiabilities shape) must reach the lake as the same
// post-states the SDK's LedgerChangeReader emits with Reason=Upgrade, each key's
// last row winning FINAL, in an identity that collides with no tx, eviction or
// snapshot-seed row.
func TestExtractLedger_UpgradeChangesMatchLedgerChangeReader(t *testing.T) {
	// One fee-only tx: the SDK reader rejects an apply-phase update with no state pre-image.
	lcm := buildParityLedger(t, 5100, []parityTx{{seed: 0x31, success: true}})

	offer := xdr.LedgerEntry{
		LastModifiedLedgerSeq: 90,
		Data: xdr.LedgerEntryData{
			Type: xdr.LedgerEntryTypeOffer,
			Offer: &xdr.OfferEntry{
				SellerId: xdr.MustAddress(parityAccount), OfferId: 77,
				Selling: xdr.MustNewNativeAsset(), Buying: xdr.MustNewCreditAsset("USD", parityAccount),
				Amount: 10, Price: xdr.Price{N: 1, D: 1},
			},
		},
	}
	offerKey, err := offer.LedgerKey()
	if err != nil {
		t.Fatal(err)
	}
	ver := xdr.Uint32(10)
	lcm.V2.UpgradesProcessing = []xdr.UpgradeEntryMeta{{
		Upgrade: xdr.LedgerUpgrade{Type: xdr.LedgerUpgradeTypeLedgerUpgradeVersion, NewLedgerVersion: &ver},
		Changes: xdr.LedgerEntryChanges{
			{Type: xdr.LedgerEntryChangeTypeLedgerEntryState, State: &offer},
			{Type: xdr.LedgerEntryChangeTypeLedgerEntryRemoved, Removed: &offerKey},
			{Type: xdr.LedgerEntryChangeTypeLedgerEntryState, State: parityChange(700).Updated},
			parityChange(650),
		},
	}}

	type post struct {
		key     string
		removed bool
		balance int64
	}
	reader, err := ingest.NewLedgerChangeReaderFromLedgerCloseMeta(parityPassphrase, lcm)
	if err != nil {
		t.Fatalf("NewLedgerChangeReaderFromLedgerCloseMeta: %v", err)
	}
	var want []post
	for {
		c, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("LedgerChangeReader.Read: %v", err)
		}
		if c.Reason != ingest.LedgerEntryChangeReasonUpgrade {
			continue
		}
		k, err := c.LedgerKey()
		if err != nil {
			t.Fatal(err)
		}
		kb, err := xdr.MarshalBase64(k)
		if err != nil {
			t.Fatal(err)
		}
		p := post{key: kb, removed: c.Post == nil}
		if c.Post != nil && c.Post.Data.Type == xdr.LedgerEntryTypeAccount {
			p.balance = int64(c.Post.Data.Account.Balance)
		}
		want = append(want, p)
	}
	_ = reader.Close()
	if len(want) != 2 {
		t.Fatalf("LedgerChangeReader emitted %d upgrade changes, want 2: the fixture is wrong", len(want))
	}

	ext, err := clickhouse.ExtractLedger(lcm, parityPassphrase)
	if err != nil {
		t.Fatalf("ExtractLedger: %v", err)
	}
	// Each key's FINAL winner is its highest intra_ledger_seq row.
	last := map[string]clickhouse.LedgerEntryChangeRow{}
	ids := map[[3]any]bool{}
	maxTxSeq := int64(-1)
	for _, r := range ext.Changes {
		id := [3]any{r.TxHash, r.OpIndex, r.ChangeIndex}
		if ids[id] {
			t.Errorf("rows share the identity (tx_hash %q, op_index %d, change_index %d)", r.TxHash, r.OpIndex, r.ChangeIndex)
		}
		ids[id] = true
		if r.TxHash != "" {
			maxTxSeq = max(maxTxSeq, int64(r.IntraLedgerSeq))
		}
		if l, ok := last[r.KeyXDR]; !ok || r.IntraLedgerSeq > l.IntraLedgerSeq {
			last[r.KeyXDR] = r
		}
	}
	var got []post
	for _, r := range ext.Changes {
		if r.TxHash != "" {
			continue
		}
		if r.OpIndex == -1 && r.ChangeType == "state" {
			t.Errorf("upgrade row %+v has the snapshot-seed shape", r)
		}
		if int64(r.IntraLedgerSeq) <= maxTxSeq {
			t.Errorf("upgrade row at intra_ledger_seq %d does not follow the tx phases (max %d)", r.IntraLedgerSeq, maxTxSeq)
		}
		if l := last[r.KeyXDR]; l.IntraLedgerSeq == r.IntraLedgerSeq && r.ChangeType != "state" {
			got = append(got, post{key: r.KeyXDR, removed: r.ChangeType == "removed", balance: r.Balance})
		}
	}
	byKey := func(s []post) { sort.Slice(s, func(i, j int) bool { return s[i].key < s[j].key }) }
	byKey(want)
	byKey(got)
	if len(got) != len(want) {
		t.Fatalf("lake FINAL upgrade post-states = %+v, want the reader's %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("post-state %d: lake %+v, reader %+v", i, got[i], want[i])
		}
	}
}
