package accounts

import (
	"math"
	"math/big"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
)

func fuzzAccountID(seed byte) xdr.AccountId {
	var pub xdr.Uint256
	pub[0] = seed
	pub[31] = 0x5a
	return xdr.AccountId{Type: xdr.PublicKeyTypePublicKeyTypeEd25519, Ed25519: &pub}
}

func fuzzAccountChange(ct xdr.LedgerEntryChangeType, aid xdr.AccountId, balance int64) xdr.LedgerEntryChange {
	if ct == xdr.LedgerEntryChangeTypeLedgerEntryRemoved {
		return xdr.LedgerEntryChange{Type: ct, Removed: &xdr.LedgerKey{
			Type:    xdr.LedgerEntryTypeAccount,
			Account: &xdr.LedgerKeyAccount{AccountId: aid},
		}}
	}
	entry := &xdr.LedgerEntry{Data: xdr.LedgerEntryData{
		Type: xdr.LedgerEntryTypeAccount,
		Account: &xdr.AccountEntry{
			AccountId:  aid,
			Balance:    xdr.Int64(balance),
			SeqNum:     xdr.SequenceNumber(balance ^ 0x55),
			HomeDomain: "example.org",
			Flags:      3,
		},
	}}
	c := xdr.LedgerEntryChange{Type: ct}
	switch ct {
	case xdr.LedgerEntryChangeTypeLedgerEntryCreated:
		c.Created = entry
	case xdr.LedgerEntryChangeTypeLedgerEntryUpdated:
		c.Updated = entry
	default:
		c.Restored = entry
	}
	return c
}

// FuzzObserverDecode drives arbitrary LedgerEntryChange XDR through the
// observer. Properties: Matches fires exactly for a watched AccountEntry
// in any change variant; a matched change always decodes to one
// Observation whose balance is the entry's int64 stroops verbatim
// (ADR-0003: no truncation or sign change), and a removal carries a zero
// balance with IsRemoval set.
func FuzzObserverDecode(f *testing.F) {
	watchedID, otherID := fuzzAccountID(0x01), fuzzAccountID(0x02)
	for _, ct := range []xdr.LedgerEntryChangeType{
		xdr.LedgerEntryChangeTypeLedgerEntryCreated,
		xdr.LedgerEntryChangeTypeLedgerEntryUpdated,
		xdr.LedgerEntryChangeTypeLedgerEntryRestored,
		xdr.LedgerEntryChangeTypeLedgerEntryRemoved,
	} {
		for _, bal := range []int64{0, 1, math.MaxInt64, math.MinInt64} {
			for _, aid := range []xdr.AccountId{watchedID, otherID} {
				b, err := fuzzAccountChange(ct, aid, bal).MarshalBinary()
				if err != nil {
					f.Fatal(err)
				}
				f.Add(b)
			}
		}
	}

	watched := watchedID.Address()
	obs, err := NewObserver([]string{watched, fuzzAccountID(0x03).Address()})
	if err != nil {
		f.Fatal(err)
	}
	closedAt := time.Unix(1_700_000_000, 0).UTC()

	f.Fuzz(func(t *testing.T, changeXDR []byte) {
		var change xdr.LedgerEntryChange
		if err := xdr.SafeUnmarshal(changeXDR, &change); err != nil {
			return
		}
		id, idErr := accountIDFromChange(change)
		wantMatch := idErr == nil && (id == watched || id == fuzzAccountID(0x03).Address())
		if got := obs.Matches(change); got != wantMatch {
			t.Fatalf("Matches=%v, want %v (id=%q err=%v)", got, wantMatch, id, idErr)
		}
		if !wantMatch {
			return
		}

		evs, err := obs.Decode(dispatcher.LedgerEntryChangeContext{
			Ledger: 11, ClosedAt: closedAt, IntraLedgerSeq: 4, Change: change,
		})
		if err != nil {
			t.Fatalf("matched change failed to decode: %v", err)
		}
		if len(evs) != 1 {
			t.Fatalf("got %d events, want 1", len(evs))
		}
		o, ok := evs[0].(Observation)
		if !ok {
			t.Fatalf("event is %T, want Observation", evs[0])
		}
		if o.AccountID != id || o.Ledger != 11 || !o.ObservedAt.Equal(closedAt) || o.IntraLedgerSeq != 4 {
			t.Fatalf("bad envelope %+v", o)
		}
		if change.Type == xdr.LedgerEntryChangeTypeLedgerEntryRemoved {
			if !o.IsRemoval || o.Balance.Sign() != 0 || o.HomeDomain != "" {
				t.Fatalf("removal observation %+v, want IsRemoval with zero balance", o)
			}
			return
		}
		var entry xdr.LedgerEntry
		switch change.Type {
		case xdr.LedgerEntryChangeTypeLedgerEntryCreated:
			entry = change.MustCreated()
		case xdr.LedgerEntryChangeTypeLedgerEntryUpdated:
			entry = change.MustUpdated()
		default:
			entry = change.MustRestored()
		}
		acct := entry.Data.MustAccount()
		if o.IsRemoval {
			t.Fatalf("%s change decoded as a removal", change.Type)
		}
		if o.Balance.Cmp(big.NewInt(int64(acct.Balance))) != 0 {
			t.Fatalf("balance %s, want %d", o.Balance, acct.Balance)
		}
		if o.HomeDomain != string(acct.HomeDomain) || o.Flags != uint32(acct.Flags) || o.SeqNum != int64(acct.SeqNum) {
			t.Fatalf("fields %+v, want home=%q flags=%d seq=%d", o, acct.HomeDomain, acct.Flags, acct.SeqNum)
		}
	})
}
