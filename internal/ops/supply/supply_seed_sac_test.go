package supply

import "testing"

// TestSacSeedTally_ObserveTracksMinMaxLedger covers the min/max-ledger
// bookkeeping that feeds sac_balance_seed_provenance.min_ledger_seen /
// max_ledger_seen (migration 0102) — the evidence that a -full-history
// pass actually reached below the ~62M current-state floor, not just a
// source-label claim.
func TestSacSeedTally_ObserveTracksMinMaxLedger(t *testing.T) {
	tally := &sacSeedTally{}
	if tally.haveLedgerBounds {
		t.Fatal("zero-value tally should not have ledger bounds yet")
	}

	tally.observe(50_000_000)
	if !tally.haveLedgerBounds {
		t.Fatal("haveLedgerBounds should be true after the first observe")
	}
	if tally.minLedger != 50_000_000 || tally.maxLedger != 50_000_000 {
		t.Fatalf("after first observe: min=%d max=%d, want both 50000000", tally.minLedger, tally.maxLedger)
	}

	// A lower ledger (the dormant, pre-floor holder) pulls the min down.
	tally.observe(41_500_000)
	if tally.minLedger != 41_500_000 {
		t.Errorf("min = %d, want 41500000 (lower ledger should update the min)", tally.minLedger)
	}
	if tally.maxLedger != 50_000_000 {
		t.Errorf("max = %d, want unchanged 50000000", tally.maxLedger)
	}

	// A higher ledger pulls the max up.
	tally.observe(69_000_000)
	if tally.maxLedger != 69_000_000 {
		t.Errorf("max = %d, want 69000000 (higher ledger should update the max)", tally.maxLedger)
	}
	if tally.minLedger != 41_500_000 {
		t.Errorf("min = %d, want unchanged 41500000", tally.minLedger)
	}

	// A ledger strictly between the current bounds changes neither.
	tally.observe(55_000_000)
	if tally.minLedger != 41_500_000 || tally.maxLedger != 69_000_000 {
		t.Errorf("mid-range observe changed bounds: min=%d max=%d, want 41500000/69000000", tally.minLedger, tally.maxLedger)
	}
}
