package v1

import "testing"

// bandGenesisLedger is Band's first on-chain write. Band emits no events, so it comes from `contract_instance_changes`,
// corroborated by ledger_entry_changes and the WASM audit. A constant of 60,000,000 would drop 9.16M ledgers from every
// completeness and gap check, letting Band read CLEAN over a window missing most of its history.
const bandGenesisLedger = 50_842_736

// TestBandGenesis_ProtocolRegistry pins Band's genesis ledger in the protocol directory, a site not covered by the other tests
// that pin the shared constant.
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
