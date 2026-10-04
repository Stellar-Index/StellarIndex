// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package entrywalk

import (
	"reflect"
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"
)

const (
	acctA byte = 0x0A
	acctB byte = 0x0B
)

func acct(seed byte, balance int64) xdr.LedgerEntry {
	id, err := xdr.NewAccountId(xdr.PublicKeyTypePublicKeyTypeEd25519, xdr.Uint256{seed})
	if err != nil {
		panic(err)
	}
	return xdr.LedgerEntry{Data: xdr.LedgerEntryData{
		Type:    xdr.LedgerEntryTypeAccount,
		Account: &xdr.AccountEntry{AccountId: id, Balance: xdr.Int64(balance)},
	}}
}

func state(e xdr.LedgerEntry) xdr.LedgerEntryChange {
	return xdr.LedgerEntryChange{Type: xdr.LedgerEntryChangeTypeLedgerEntryState, State: &e}
}

func updated(e xdr.LedgerEntry) xdr.LedgerEntryChange {
	return xdr.LedgerEntryChange{Type: xdr.LedgerEntryChangeTypeLedgerEntryUpdated, Updated: &e}
}

// Two exports of one ledger list a block's keys in different orders; both
// must come out identical, with each key's state still ahead of its update.
func TestCanonical_KeyOrderIndependentAndPairsKept(t *testing.T) {
	aState, aUpd := state(acct(acctA, 10)), updated(acct(acctA, 5))
	bState, bUpd := state(acct(acctB, 20)), updated(acct(acctB, 25))

	one := Canonical([]xdr.LedgerEntryChange{aState, aUpd, bState, bUpd})
	two := Canonical([]xdr.LedgerEntryChange{bState, bUpd, aState, aUpd})
	if !reflect.DeepEqual(one, two) {
		t.Fatalf("the same block in two key orders canonicalised differently:\n%v\n%v", one, two)
	}
	for i := 0; i < len(one); i += 2 {
		if one[i].Type != xdr.LedgerEntryChangeTypeLedgerEntryState || one[i+1].Type != xdr.LedgerEntryChangeTypeLedgerEntryUpdated {
			t.Fatalf("pair %d = %v/%v, want state then updated for one key", i/2, one[i].Type, one[i+1].Type)
		}
		ka, _ := one[i].LedgerKey()
		kb, _ := one[i+1].LedgerKey()
		if !ka.Equals(kb) {
			t.Fatalf("pair %d split across keys", i/2)
		}
	}
}

func TestCanonical_DoesNotModifyInput(t *testing.T) {
	in := []xdr.LedgerEntryChange{updated(acct(acctB, 1)), updated(acct(acctA, 2))}
	first := in[0]
	out := Canonical(in)
	if !reflect.DeepEqual(in[0], first) {
		t.Fatal("Canonical reordered the caller's slice; the ledger meta is shared with other readers")
	}
	if reflect.DeepEqual(out, in) {
		t.Fatal("expected a reordered copy for an unsorted block")
	}
}
