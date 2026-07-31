package dispatcher

import (
	"testing"

	"github.com/stellar/go-stellar-sdk/ingest"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// contract IDs used across the state-write-key tests.
var (
	swkContractA = xdr.ContractId{0xAA, 0x01}
	swkContractB = xdr.ContractId{0xBB, 0x02}
)

// swkDataEntry builds a contract-data LedgerEntry for contract `cid` with
// an ScString key and a u64 value — the value is what the changed-write
// rule compares.
func swkDataEntry(cid xdr.ContractId, key string, val uint64) xdr.LedgerEntry {
	skey := xdr.ScString(key)
	v := xdr.Uint64(val)
	return xdr.LedgerEntry{
		Data: xdr.LedgerEntryData{
			Type: xdr.LedgerEntryTypeContractData,
			ContractData: &xdr.ContractDataEntry{
				Contract: xdr.ScAddress{
					Type:       xdr.ScAddressTypeScAddressTypeContract,
					ContractId: &cid,
				},
				Key:        xdr.ScVal{Type: xdr.ScValTypeScvString, Str: &skey},
				Durability: xdr.ContractDataDurabilityPersistent,
				Val:        xdr.ScVal{Type: xdr.ScValTypeScvU64, U64: &v},
			},
		},
	}
}

func swkChange(t xdr.LedgerEntryChangeType, entry xdr.LedgerEntry) xdr.LedgerEntryChange {
	c := xdr.LedgerEntryChange{Type: t}
	switch t {
	case xdr.LedgerEntryChangeTypeLedgerEntryCreated:
		c.Created = &entry
	case xdr.LedgerEntryChangeTypeLedgerEntryUpdated:
		c.Updated = &entry
	case xdr.LedgerEntryChangeTypeLedgerEntryState:
		c.State = &entry
	}
	return c
}

func swkTrustlineChange() xdr.LedgerEntryChange {
	entry := xdr.LedgerEntry{
		Data: xdr.LedgerEntryData{
			Type: xdr.LedgerEntryTypeTrustline,
			TrustLine: &xdr.TrustLineEntry{
				AccountId: xdr.MustAddress("GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAWHF"),
				Asset:     xdr.TrustLineAsset{Type: xdr.AssetTypeAssetTypeNative},
			},
		},
	}
	return swkChange(xdr.LedgerEntryChangeTypeLedgerEntryUpdated, entry)
}

func swkTx(v int32, opChanges ...[]xdr.LedgerEntryChange) ingest.LedgerTransaction {
	meta := xdr.TransactionMeta{V: v}
	switch v {
	case 3:
		ops := make([]xdr.OperationMeta, len(opChanges))
		for i, ch := range opChanges {
			ops[i] = xdr.OperationMeta{Changes: ch}
		}
		meta.V3 = &xdr.TransactionMetaV3{Operations: ops}
	case 4:
		ops := make([]xdr.OperationMetaV2, len(opChanges))
		for i, ch := range opChanges {
			ops[i] = xdr.OperationMetaV2{Changes: ch}
		}
		meta.V4 = &xdr.TransactionMetaV4{Operations: ops}
	}
	return ingest.LedgerTransaction{UnsafeMeta: meta}
}

// The redstone-shaped case, mirroring r1 ledger 62056824's tx 40758bde:
// one feed's entry changes value (accepted), the others are rewritten
// byte-identical (rejected) — only the changed key survives.
func TestOpContractDataWriteKeys_ValueChangedOnly(t *testing.T) {
	tx := swkTx(3, []xdr.LedgerEntryChange{
		// accepted: value 1 → 2
		swkChange(xdr.LedgerEntryChangeTypeLedgerEntryState, swkDataEntry(swkContractA, "iBENJI", 1)),
		swkChange(xdr.LedgerEntryChangeTypeLedgerEntryUpdated, swkDataEntry(swkContractA, "iBENJI", 2)),
		// rejected: identical-value rewrite
		swkChange(xdr.LedgerEntryChangeTypeLedgerEntryState, swkDataEntry(swkContractA, "PYUSD", 7)),
		swkChange(xdr.LedgerEntryChangeTypeLedgerEntryUpdated, swkDataEntry(swkContractA, "PYUSD", 7)),
		// created (no pre-image) counts as changed
		swkChange(xdr.LedgerEntryChangeTypeLedgerEntryCreated, swkDataEntry(swkContractA, "USTRY", 9)),
		// non-contract-data — excluded
		swkTrustlineChange(),
		// other contract, changed — kept here, filtered per event below
		swkChange(xdr.LedgerEntryChangeTypeLedgerEntryCreated, swkDataEntry(swkContractB, "other", 3)),
	})

	writes := opContractDataWriteKeys(tx, 0)
	if len(writes) != 3 {
		t.Fatalf("got %d changed writes, want 3 (iBENJI, USTRY, B/other): %+v", len(writes), writes)
	}
	strA, err := contractIDToStrkey(swkContractA)
	if err != nil {
		t.Fatal(err)
	}
	strB, err := contractIDToStrkey(swkContractB)
	if err != nil {
		t.Fatal(err)
	}

	// Per-event filter: each contract's events see only their own keys.
	keysA := stateWriteKeysFor(writes, strA)
	if len(keysA) != 2 {
		t.Fatalf("keysA = %v, want iBENJI + USTRY", keysA)
	}
	for i, wantFeed := range []string{"iBENJI", "USTRY"} {
		var lk xdr.LedgerKey
		if err := xdr.SafeUnmarshalBase64(keysA[i], &lk); err != nil {
			t.Fatalf("key %d round-trip: %v", i, err)
		}
		cd, ok := lk.GetContractData()
		if !ok {
			t.Fatalf("key %d type = %v, want contract data", i, lk.Type)
		}
		if s, _ := cd.Key.GetStr(); string(s) != wantFeed {
			t.Fatalf("keysA[%d] = %q, want %q (first-write order, PYUSD excluded as unchanged)", i, s, wantFeed)
		}
	}
	if keysB := stateWriteKeysFor(writes, strB); len(keysB) != 1 {
		t.Fatalf("keysB = %v, want exactly contract B's key", keysB)
	}
	if keys := stateWriteKeysFor(writes, "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"); keys != nil {
		t.Fatalf("unrelated contract got keys %v, want nil", keys)
	}
}

func TestOpContractDataWriteKeys_MetaV4AndBounds(t *testing.T) {
	changed := []xdr.LedgerEntryChange{
		swkChange(xdr.LedgerEntryChangeTypeLedgerEntryState, swkDataEntry(swkContractA, "USTRY", 1)),
		swkChange(xdr.LedgerEntryChangeTypeLedgerEntryUpdated, swkDataEntry(swkContractA, "USTRY", 2)),
	}
	tx := swkTx(4, changed)
	if writes := opContractDataWriteKeys(tx, 0); len(writes) != 1 {
		t.Fatalf("V4 writes = %+v, want 1", writes)
	}
	if writes := opContractDataWriteKeys(tx, 1); writes != nil {
		t.Fatalf("out-of-range op returned %+v, want nil", writes)
	}
	if writes := opContractDataWriteKeys(ingest.LedgerTransaction{UnsafeMeta: xdr.TransactionMeta{V: 2}}, 0); writes != nil {
		t.Fatalf("unknown meta version returned %+v, want nil", writes)
	}
}
