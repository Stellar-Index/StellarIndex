package clickhouse

import (
	"strings"
	"testing"
)

// Every served table is rebuilt from empty and swapped at the end: a
// staging twin that is filled but never truncated would accumulate
// cycles, and one that is never exchanged would never be read.
func TestCohortRollupRebuildsAndSwapsEveryServedTable(t *testing.T) {
	last := cohortRollupStatements[len(cohortRollupStatements)-1].sql
	if !strings.HasPrefix(last, "EXCHANGE TABLES") {
		t.Fatalf("last statement must be the swap, got %q", last[:40])
	}
	for _, table := range cohortStagingTables {
		staging := "stellar." + table + "_staging"
		truncated := false
		for _, step := range cohortRollupStatements {
			if strings.HasPrefix(step.sql, "TRUNCATE TABLE "+staging) {
				truncated = true
			}
		}
		// The cohortLoadedTables' staging twins are truncated by their
		// loaders (loadDeFiPositionHolders, loadAssetMonthUSDPrices) before
		// the statements run, because those snapshots are batch-inserted
		// from Go rather than filled by SQL.
		if !truncated && !cohortLoadedTables[table] {
			t.Errorf("%s is never truncated before the cycle fills it", staging)
		}
		if !strings.Contains(last, staging+" AND stellar."+table) {
			t.Errorf("%s is not exchanged live: %s", staging, last)
		}
	}
}

// The movements archive is 10B+ rows: it is read exactly once, in ledger
// windows, and only into the parts table — every served figure derived
// from it is a fold over that table, never a second pass.
func TestCohortRollupWalksMovementsOnceIntoParts(t *testing.T) {
	var touching []int
	for i, step := range cohortRollupStatements {
		if strings.Contains(step.sql, "stellar.account_movements") {
			touching = append(touching, i)
		}
	}
	if len(touching) != 1 {
		t.Fatalf("statements reading stellar.account_movements: %v, want exactly one", touching)
	}
	step := cohortRollupStatements[touching[0]]
	if !step.walk || step.windowBinds != 1 {
		t.Errorf("the movements pass must be a windowed walk with one BETWEEN pair (walk=%v binds=%d)", step.walk, step.windowBinds)
	}
	if !strings.Contains(step.sql, "INSERT INTO stellar.account_cohort_parts_staging") {
		t.Error("the movements pass must fill the parts table, not a served table")
	}
	if !strings.Contains(step.sql, "ledger BETWEEN ? AND ?") {
		t.Error("the movements pass must be bounded by the window binds")
	}
	if !strings.Contains(step.sql, "uniqCombinedState(m.address)") {
		t.Error("the parts row must carry a MERGEABLE distinct-member state; a month spans windows")
	}
	// The archive is a ReplacingMergeTree, so the window must de-duplicate
	// — with FINAL, per partition. The boards' argMax GROUP BY exceeds the
	// 8 GiB budget on a dense window (measured; see cohortScanSettings).
	if !strings.Contains(step.sql, "FROM stellar.account_movements FINAL") {
		t.Error("the movements window must read the archive with FINAL")
	}
	if strings.Contains(step.sql, "argMax(") {
		t.Error("the movements window must not de-duplicate with an argMax GROUP BY; it does not fit the memory budget")
	}
	if !strings.Contains(step.sql, "do_not_merge_across_partitions_select_final = 1") {
		t.Error("FINAL must be told not to merge across partitions — a window is one partition")
	}
}

// A month's figure is the merge of its window partials. Summing a
// per-window distinct count would overstate active accounts by the
// number of windows a month spans.
func TestCohortRollupFoldsMergeDistinctStates(t *testing.T) {
	folds := 0
	for _, step := range cohortRollupStatements {
		if !strings.Contains(step.sql, "FROM stellar.account_cohort_parts_staging") {
			continue
		}
		folds++
		if !strings.Contains(step.sql, "uniqCombinedMerge(actives)") {
			t.Errorf("fold over the parts table does not merge the distinct-member state:\n%s", step.sql)
		}
		if strings.Contains(step.sql, "sum(active") {
			t.Errorf("fold sums a distinct count across windows:\n%s", step.sql)
		}
	}
	if folds != 3 {
		t.Errorf("folds over the parts table: %d, want 3 (per-asset flows, all-assets flows, contracts)", folds)
	}
}

// No statement decodes an operation body or an entry XDR: every column
// read is one the lake already materialised.
func TestCohortRollupReadsNoXDR(t *testing.T) {
	for i, step := range cohortRollupStatements {
		for _, col := range []string{"body_xdr", "entry_xdr", "key_xdr", "base64Decode"} {
			if strings.Contains(step.sql, col) {
				t.Errorf("statement %d reads %s", i+1, col)
			}
		}
	}
}

// Membership decides everything downstream, so it is built first, from
// the edge tables the boards already serve, with the creator floor
// applied where the edges are selected — not in each consumer.
func TestCohortRollupMembershipFloorsCreatorsOnce(t *testing.T) {
	first := cohortRollupStatements[1].sql
	if !strings.Contains(first, "INSERT INTO stellar.account_cohort_members") {
		t.Fatalf("membership must be the first fill, got: %s", first[:60])
	}
	if !strings.Contains(first, "accounts_created >= "+itoa(AccountCohortMinCreated)) {
		t.Error("the creator floor is not applied when membership is built")
	}
	if !strings.Contains(first, "FROM stellar.account_sponsor_edges") || !strings.Contains(first, "FROM stellar.account_creator_edges") {
		t.Error("membership must come from both edge tables")
	}
	for i, step := range cohortRollupStatements[2:] {
		if strings.Contains(step.sql, "accounts_created >=") {
			t.Errorf("statement %d re-applies the creator floor; membership already did", i+3)
		}
	}
}

// The positions join reads the snapshot's STAGING table — the one
// RunCohortRollup fills this cycle — never the live one from the last
// cycle, which would pair this cycle's membership with last cycle's
// positions.
func TestCohortRollupPositionsJoinReadsThisCyclesSnapshot(t *testing.T) {
	seen := false
	for _, step := range cohortRollupStatements {
		if !strings.Contains(step.sql, "INSERT INTO stellar.account_cohort_positions_staging") {
			continue
		}
		seen = true
		if !strings.Contains(step.sql, "FROM stellar.defi_position_holders_staging") {
			t.Errorf("positions join must read defi_position_holders_staging:\n%s", step.sql)
		}
	}
	if !seen {
		t.Fatal("no statement fills account_cohort_positions_staging")
	}
}

// The big joins are told to spill: the membership side is ~25M rows on
// pubnet and a hash join sized in memory is the OOM the rollup budget
// exists to prevent.
func TestCohortRollupBigJoinsSpill(t *testing.T) {
	for i, step := range cohortRollupStatements {
		joinsMembers := strings.Contains(step.sql, "INNER JOIN stellar.account_cohort_members")
		if !joinsMembers {
			continue
		}
		if strings.Contains(step.sql, "defi_position_holders_staging") {
			continue // a few hundred thousand rows: in-memory is right
		}
		if !strings.Contains(step.sql, "join_algorithm = 'grace_hash'") {
			t.Errorf("statement %d joins the membership without a spilling join algorithm", i+1)
		}
	}
}

func TestCohortExchangeSQLNamesEveryPair(t *testing.T) {
	got := cohortExchangeSQL()
	if strings.Count(got, " AND ") != len(cohortStagingTables) {
		t.Errorf("exchange pairs: %d, want %d: %s", strings.Count(got, " AND "), len(cohortStagingTables), got)
	}
}
