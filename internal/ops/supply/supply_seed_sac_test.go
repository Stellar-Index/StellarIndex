package supply

import (
	"math/big"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
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

// TestUnmatchedSACWrappersErr — a watched wrapper that matched no Balance key
// fails the pass by name; one whose keys were all retracted did match.
func TestUnmatchedSACWrappersErr(t *testing.T) {
	watched := map[string]string{"CLIVE": "LIVE:G1", "CGONE": "GONE:G2", "CNONE": "NONE:G3"}
	tallies := map[string]*sacSeedTally{
		"CLIVE": {holders: 3, sum: big.NewInt(9)},
		"CGONE": {retracted: 2, sum: big.NewInt(0)},
	}
	err := unmatchedSACWrappersErr(watched, tallies)
	if err == nil {
		t.Fatal("err = nil, want the zero-match wrapper refused")
	}
	if got := err.Error(); !strings.Contains(got, "1 watched SAC wrapper(s)") || !strings.Contains(got, "CNONE (NONE:G3)") || strings.Contains(got, "CGONE") {
		t.Errorf("err = %q, want exactly CNONE named", got)
	}

	delete(watched, "CNONE")
	if err := unmatchedSACWrappersErr(watched, tallies); err != nil {
		t.Errorf("err = %v, want nil when every wrapper matched", err)
	}
}

// TestCheckSACSeedShrink — a re-seed over a shrunken key-set must not
// overwrite provenance: holders the previous same-source pass seeded that this
// pass neither re-seeds nor retracts keep serving their old balance.
func TestCheckSACSeedShrink(t *testing.T) {
	full := timescale.SACBalanceSeedSourceFullHistory
	prev := timescale.SACBalanceSeedProvenance{ContractID: "CPHO", Source: full, HoldersSeeded: 45}

	// 6 live + 39 retracted accounts for every previously-seeded holder.
	if err := checkSACSeedShrink(prev, full, &sacSeedTally{holders: 6, retracted: 39}); err != nil {
		t.Errorf("retracted holders accounted for: err = %v, want nil", err)
	}
	// 39 holders vanished without a tombstone.
	err := checkSACSeedShrink(prev, full, &sacSeedTally{holders: 6})
	if err == nil || !strings.Contains(err.Error(), "39 were neither re-seeded nor retracted") {
		t.Errorf("shrunken key-set: err = %v, want the 39 unaccounted holders refused", err)
	}
	// current_state cannot see below its floor; not comparable to full_history.
	if err := checkSACSeedShrink(prev, timescale.SACBalanceSeedSourceCurrentState, &sacSeedTally{holders: 6}); err != nil {
		t.Errorf("cross-source: err = %v, want nil", err)
	}
}

// TestSACSeedProvenance_FullHistoryNeedsLakeEvidence — GH #713: the
// full_history stamp comes from what the walk proved, not from -full-history.
// A pass with no lake verification stamps nothing; a verified one records the
// ledger it was verified through and its retractions.
func TestSACSeedProvenance_FullHistoryNeedsLakeEvidence(t *testing.T) {
	full := timescale.SACBalanceSeedSourceFullHistory
	tally := &sacSeedTally{holders: 6, retracted: 39, sum: big.NewInt(1)}
	tally.observe(41_500_000)

	if _, err := sacSeedProvenance("CPHO", "PHO:G1", full, clickhouse.SeedEvidence{FromLedger: 2, ToLedger: 63_000_000}, tally); err == nil {
		t.Fatal("an unverified walk produced a full_history provenance row")
	}

	p, err := sacSeedProvenance("CPHO", "PHO:G1", full, clickhouse.SeedEvidence{FromLedger: 2, ToLedger: 63_000_000, LakeVerifiedThrough: 63_000_000}, tally)
	if err != nil {
		t.Fatal(err)
	}
	if p.LakeVerifiedThrough == nil || *p.LakeVerifiedThrough != 63_000_000 {
		t.Errorf("LakeVerifiedThrough = %v, want 63000000", p.LakeVerifiedThrough)
	}
	if p.HoldersRetracted == nil || *p.HoldersRetracted != 39 || p.HoldersSeeded != 6 {
		t.Errorf("HoldersSeeded=%d HoldersRetracted=%v, want 6 and 39", p.HoldersSeeded, p.HoldersRetracted)
	}

	cs, err := sacSeedProvenance("CPHO", "PHO:G1", timescale.SACBalanceSeedSourceCurrentState, clickhouse.SeedEvidence{}, tally)
	if err != nil || cs.LakeVerifiedThrough != nil {
		t.Errorf("current_state: err=%v LakeVerifiedThrough=%v, want a row with no coverage claim", err, cs.LakeVerifiedThrough)
	}
}
