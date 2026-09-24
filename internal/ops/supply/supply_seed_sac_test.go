package supply

import (
	"math/big"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// TestSupplySeedSACBalances_AcceptsTimeout pins the -timeout flag: the
// full-history pass writes only after the whole lake scan, so its deadline must
// be operator-settable rather than a fixed budget that silently loses the pass.
func TestSupplySeedSACBalances_AcceptsTimeout(t *testing.T) {
	err := supplySeedSACBalances([]string{"-timeout", "20h"})
	if err == nil || err.Error() != "-config is required" {
		t.Fatalf("err = %v, want flag parsing to accept -timeout and stop at the missing -config", err)
	}
}

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

// TestSacSeedTally_TombstonesAreNotHolders — a retraction tombstone is counted
// apart: it must not inflate holders_seeded, the summed balance, or the
// min/max_ledger_seen provenance bounds.
func TestSacSeedTally_TombstonesAreNotHolders(t *testing.T) {
	tally := &sacSeedTally{sum: big.NewInt(0)}
	tally.add(clickhouse.SACBalanceSeed{Balance: big.NewInt(250), LedgerSeq: 50_000_000})
	tally.add(clickhouse.SACBalanceSeed{Balance: big.NewInt(0), LedgerSeq: 41_000_000, IsRemoval: true})
	tally.add(clickhouse.SACBalanceSeed{Balance: big.NewInt(0), LedgerSeq: 69_000_000, IsRemoval: true})

	if tally.holders != 1 {
		t.Errorf("holders = %d, want 1 (tombstones are not holders)", tally.holders)
	}
	if tally.retracted != 2 {
		t.Errorf("retracted = %d, want 2", tally.retracted)
	}
	if tally.sum.Cmp(big.NewInt(250)) != 0 {
		t.Errorf("sum = %s, want 250", tally.sum)
	}
	if !tally.haveLedgerBounds || tally.minLedger != 50_000_000 || tally.maxLedger != 50_000_000 {
		t.Errorf("ledger bounds = [%d, %d] (have=%v), want [50000000, 50000000] from the live holder only", tally.minLedger, tally.maxLedger, tally.haveLedgerBounds)
	}
}
