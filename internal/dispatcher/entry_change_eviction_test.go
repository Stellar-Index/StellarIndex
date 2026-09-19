package dispatcher

import (
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"
)

// evictedContractDataKey is the ledger key core reports for an archived
// contract-data entry: the entry's own key, with no body (the entry is
// gone from the live state, so there is nothing to carry).
func evictedContractDataKey(seed byte) xdr.LedgerKey {
	var raw [32]byte
	raw[0] = seed
	cid := xdr.ContractId(raw)
	sym := xdr.ScSymbol("Balance")
	return xdr.LedgerKey{
		Type: xdr.LedgerEntryTypeContractData,
		ContractData: &xdr.LedgerKeyContractData{
			Contract: xdr.ScAddress{
				Type:       xdr.ScAddressTypeScAddressTypeContract,
				ContractId: &cid,
			},
			Key:        xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym},
			Durability: xdr.ContractDataDurabilityPersistent,
		},
	}
}

// evictedTTLKey is the TTL key core evicts alongside the data key. No
// decoder watches it; it is here so the test walks the real shape of the
// list rather than a one-element idealisation of it.
func evictedTTLKey(seed byte) xdr.LedgerKey {
	var raw [32]byte
	raw[0] = seed
	return xdr.LedgerKey{
		Type: xdr.LedgerEntryTypeTtl,
		Ttl:  &xdr.LedgerKeyTtl{KeyHash: xdr.Hash(raw)},
	}
}

// TestProcessLedger_EvictedKeysReachTheEntryDecoders pins Q119
// (audit-2026-09-02). Soroban state archival is the one way a ledger entry
// leaves the live state WITHOUT a transaction touching it: when its TTL
// lapses, core evicts it at ledger close and reports it only in the
// LedgerCloseMeta's evicted-keys list. The three-phase walk reads
// transaction meta, so it could never see one.
//
// The consequence was not cosmetic. The decoders handled Restored — the
// other half of the same lifecycle — so an archived SAC balance's last
// write stood as the holder's current balance forever and the served supply
// component stayed permanently above the truth with no path to
// self-correct.
func TestProcessLedger_EvictedKeysReachTheEntryDecoders(t *testing.T) {
	env, proc := mkEntryWalkTx(t, 0x55, true, nil, []xdr.LedgerEntryChange{accountBalanceChange(1000)})
	lcm := mkEntryWalkLedger(4244,
		[]xdr.TransactionEnvelope{env},
		[]xdr.TransactionResultMeta{proc})
	dataKey := evictedContractDataKey(0x9e)
	lcm.V1.EvictedKeys = []xdr.LedgerKey{evictedTTLKey(0x9e), dataKey}

	spy := &entryChangeSpy{}
	d := New()
	d.AddEntryDecoder(spy)

	if _, err := d.ProcessLedger(lcm, testPassphrase); err != nil {
		t.Fatalf("ProcessLedger: %v", err)
	}

	var (
		evicted   *LedgerEntryChangeContext
		sawTTLKey bool
		maxTxSeq  uint32
		txChanges int
	)
	for i := range spy.seen {
		ctx := spy.seen[i]
		if ctx.Change.Type != xdr.LedgerEntryChangeTypeLedgerEntryRemoved || ctx.Change.Removed == nil {
			txChanges++
			if ctx.IntraLedgerSeq > maxTxSeq {
				maxTxSeq = ctx.IntraLedgerSeq
			}
			continue
		}
		switch ctx.Change.Removed.Type {
		case xdr.LedgerEntryTypeContractData:
			evicted = &spy.seen[i]
		case xdr.LedgerEntryTypeTtl:
			sawTTLKey = true
		default:
			t.Errorf("unexpected removed key type %v", ctx.Change.Removed.Type)
		}
	}

	if evicted == nil {
		t.Fatalf("the ledger's evicted contract-data key never reached the entry decoders; "+
			"saw %d changes — an evicted SAC balance then stays in the served supply forever",
			len(spy.seen))
	}
	if got, want := *evicted.Change.Removed.ContractData, *dataKey.ContractData; got.Contract != want.Contract ||
		got.Durability != want.Durability {
		t.Errorf("evicted change carries key %+v, want %+v", got, want)
	}
	if !sawTTLKey {
		t.Error("the paired TTL key was dropped; the walk must dispatch the eviction list as core reports it " +
			"and let each decoder's Matches discard what it does not watch")
	}
	if evicted.TxHash != "" || evicted.OpIndex != -1 {
		t.Errorf("evicted change stamped tx_hash=%q op_index=%d, want \"\" / -1 — "+
			"eviction is applied at ledger close and belongs to no transaction",
			evicted.TxHash, evicted.OpIndex)
	}
	if txChanges == 0 {
		t.Fatal("no transaction-phase change was walked; the fixture is not exercising the ordering assertion")
	}
	if evicted.IntraLedgerSeq <= maxTxSeq {
		t.Errorf("evicted change has IntraLedgerSeq %d <= the last transaction-phase change at %d: "+
			"a balance written earlier in this same ledger would win the last-writer-wins upsert "+
			"and the eviction would be discarded", evicted.IntraLedgerSeq, maxTxSeq)
	}
}

// TestProcessLedger_NoEvictionsEmitsNothingExtra keeps the negative half
// honest: a ledger with an empty eviction list must walk exactly the
// transaction-phase changes it always did. The eviction phase is additive,
// which is also why it does not renumber IntraLedgerSeq (see
// [EntryWalkVersion]).
func TestProcessLedger_NoEvictionsEmitsNothingExtra(t *testing.T) {
	env, proc := mkEntryWalkTx(t, 0x66, true,
		[]xdr.LedgerEntryChange{accountBalanceChange(990)},
		[]xdr.LedgerEntryChange{accountBalanceChange(500)})
	lcm := mkEntryWalkLedger(4245,
		[]xdr.TransactionEnvelope{env},
		[]xdr.TransactionResultMeta{proc})

	spy := &entryChangeSpy{}
	d := New()
	d.AddEntryDecoder(spy)

	if _, err := d.ProcessLedger(lcm, testPassphrase); err != nil {
		t.Fatalf("ProcessLedger: %v", err)
	}
	if len(spy.seen) != 2 {
		t.Fatalf("walked %d changes, want exactly the 2 transaction-phase changes", len(spy.seen))
	}
	for i, ctx := range spy.seen {
		if got := balanceOf(ctx.Change); got != []int64{990, 500}[i] {
			t.Errorf("change %d = balance %d, want %d (order unchanged by the eviction phase)",
				i, got, []int64{990, 500}[i])
		}
		if ctx.IntraLedgerSeq != uint32(i) {
			t.Errorf("change %d has IntraLedgerSeq %d, want %d", i, ctx.IntraLedgerSeq, i)
		}
	}
}
