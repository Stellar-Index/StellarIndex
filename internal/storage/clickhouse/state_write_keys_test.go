package clickhouse

import (
	"strings"
	"testing"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
)

func TestStateWriteKeysQuery_Shape(t *testing.T) {
	q := stateWriteKeysQuery([]opRef{
		{Ledger: 62056824, TxHash: "40758bde", OpIndex: 0},
		{Ledger: 62056830, TxHash: "ab'cd", OpIndex: 2},
	})
	for _, want := range []string{
		"FROM stellar.ledger_entry_changes",
		"ledger_seq BETWEEN 62056824 AND 62056830",
		"entry_type = 'contract_data'",
		"change_type IN ('state','created','updated')",
		"(62056824,'40758bde',0)",
		`(62056830,'ab\'cd',2)`, // lake-sourced literal is escaped
		"ORDER BY ledger_seq, tx_hash, op_index, change_index",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("query missing %q:\n%s", want, q)
		}
	}
}

// swkEntry marshals a contract-data LedgerEntry (ScString key, u64 val)
// plus its LedgerKey, both base64 — the lake's entry_xdr / key_xdr pair.
func swkEntry(t *testing.T, cid xdr.ContractId, key string, val uint64) (keyB64, entryB64 string) {
	t.Helper()
	skey := xdr.ScString(key)
	v := xdr.Uint64(val)
	entry := xdr.LedgerEntry{
		Data: xdr.LedgerEntryData{
			Type: xdr.LedgerEntryTypeContractData,
			ContractData: &xdr.ContractDataEntry{
				Contract:   xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &cid},
				Key:        xdr.ScVal{Type: xdr.ScValTypeScvString, Str: &skey},
				Durability: xdr.ContractDataDurabilityPersistent,
				Val:        xdr.ScVal{Type: xdr.ScValTypeScvU64, U64: &v},
			},
		},
	}
	lk, err := entry.LedgerKey()
	if err != nil {
		t.Fatal(err)
	}
	keyB64, err = xdr.MarshalBase64(lk)
	if err != nil {
		t.Fatal(err)
	}
	entryB64, err = xdr.MarshalBase64(entry)
	if err != nil {
		t.Fatal(err)
	}
	return keyB64, entryB64
}

// Mirrors r1 ledger 62056824's tx 40758bde: the accepted feed's value
// changes, rejected feeds are rewritten byte-identical, and only keys of
// the event's own contract count.
func TestChangedWriteKeysForContract(t *testing.T) {
	cidA := xdr.ContractId{0xAA, 0x01}
	cidB := xdr.ContractId{0xBB, 0x02}
	strA, err := strkey.Encode(strkey.VersionByteContract, cidA[:])
	if err != nil {
		t.Fatal(err)
	}

	kAccept, ePre := swkEntry(t, cidA, "iBENJI", 1)
	_, ePost := swkEntry(t, cidA, "iBENJI", 2)
	kReject, eSame := swkEntry(t, cidA, "PYUSD", 7)
	kCreate, eNew := swkEntry(t, cidA, "USTRY", 9)
	kForeign, eForeign := swkEntry(t, cidB, "other", 3)

	rows := []entryChangeLite{
		{ChangeIndex: 4, ChangeType: "state", KeyXDR: kAccept, EntryXDR: ePre},
		{ChangeIndex: 5, ChangeType: "updated", KeyXDR: kAccept, EntryXDR: ePost},
		{ChangeIndex: 6, ChangeType: "state", KeyXDR: kReject, EntryXDR: eSame},
		{ChangeIndex: 7, ChangeType: "updated", KeyXDR: kReject, EntryXDR: eSame},
		{ChangeIndex: 8, ChangeType: "created", KeyXDR: kCreate, EntryXDR: eNew},
		{ChangeIndex: 9, ChangeType: "created", KeyXDR: kForeign, EntryXDR: eForeign},
		{ChangeIndex: 10, ChangeType: "updated", KeyXDR: "garbage!!", EntryXDR: "garbage!!"},
	}

	keys := changedWriteKeysForContract(rows, strA)
	if len(keys) != 2 || keys[0] != kAccept || keys[1] != kCreate {
		t.Fatalf("keys = %v, want [iBENJI(changed), USTRY(created)] — PYUSD unchanged, foreign+garbage dropped", keys)
	}
	if keys := changedWriteKeysForContract(rows, "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"); keys != nil {
		t.Fatalf("unrelated contract got %v, want nil", keys)
	}
	if keys := changedWriteKeysForContract(nil, strA); keys != nil {
		t.Fatalf("empty rows got %v, want nil", keys)
	}
}
