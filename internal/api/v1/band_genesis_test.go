package v1

import "testing"

// bandGenesisLedger is Band's first on-chain write.
//
// It is NOT discoverable the obvious way: Band's Soroban contract emits
// zero events, so `min(ledger_seq) FROM contract_events` returns 0 rows
// and reads as "no data" rather than "wrong table". The number comes from
// `contract_instance_changes` (whose own floor is 50,457,429, so this is
// Band's first write and not the table's edge), corroborated by 4,210
// contract_data writes in `ledger_entry_changes` from the same ledger and
// by the WASM audit.
//
// Two of the four constants that carry it said 60,000,000 until #361/#363.
// The 9.16M-ledger difference shortened the range every completeness and
// gap check evaluated for Band, so the source could read CLEAN over a
// window that excluded most of its history — the worst shape for a
// coverage claim, because it passes.
const bandGenesisLedger = 50_842_736

// TestBandGenesis_ProtocolRegistry pins the copy in the protocol
// directory. a368b5c4's message claimed a single test pinned all four
// sites; an independent review reverted this one and the whole suite
// stayed green, so the claim was false and these assertions are the fix.
func TestBandGenesis_ProtocolRegistry(t *testing.T) {
	for _, p := range protocolRegistry {
		if p.Name != "band" {
			continue
		}
		if p.GenesisLedger != bandGenesisLedger {
			t.Errorf("protocolRegistry band GenesisLedger = %d, want %d — "+
				"an event-table probe returns 0 rows for Band (it emits none), so a value "+
				"'corrected' that way is vacuous; use contract_instance_changes",
				p.GenesisLedger, bandGenesisLedger)
		}
		return
	}
	t.Fatal("band is not in protocolRegistry")
}

// TestBandGenesis_DiagnosticsMap pins the third copy.
func TestBandGenesis_DiagnosticsMap(t *testing.T) {
	got, ok := sourceGenesisLedger["band"]
	if !ok {
		t.Fatal("band is not in sourceGenesisLedger")
	}
	if got != bandGenesisLedger {
		t.Errorf("sourceGenesisLedger[band] = %d, want %d", got, bandGenesisLedger)
	}
}
