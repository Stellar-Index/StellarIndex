package clickhouse

import (
	"context"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
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

// ClickHouse sum() keeps its argument's width and wraps on overflow, so a
// toInt128 around an aggregate widens a value that has already wrapped:
// every money sum in this package must widen its argument instead.
func TestNoAggregateThenWidenInPackageSQL(t *testing.T) {
	widenAfter := regexp.MustCompile(`toInt(128|256)\(\s*sum(If)?\(`)
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	scanned := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		scanned++
		for _, loc := range widenAfter.FindAllIndex(src, -1) {
			line := 1 + strings.Count(string(src[:loc[0]]), "\n")
			t.Errorf("%s:%d widens after summing (%s): write sum(toInt128(x))", f, line, src[loc[0]:loc[1]])
		}
	}
	if scanned == 0 {
		t.Fatal("scanned no package sources")
	}
}

// cohortFlowsFakeRows scripts one row per Scan call for readCohortFlows;
// the column shape mirrors both cohortFlowsSQL and cohortFlowsFallbackSQL
// (they always Scan the same 7 destinations).
type cohortFlowsFakeRows struct {
	driver.Rows
	i int
}

func (r *cohortFlowsFakeRows) Next() bool {
	r.i++
	return r.i == 1
}

func (r *cohortFlowsFakeRows) Scan(dest ...any) error {
	*dest[0].(*time.Time) = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	*dest[1].(*string) = "native"
	*dest[2].(*big.Int) = *big.NewInt(100)
	*dest[3].(*big.Int) = *big.NewInt(40)
	*dest[4].(*uint64) = 5
	*dest[5].(*uint64) = 2
	*dest[6].(*string) = "" // no then-price either way in this fixture
	return nil
}

func (r *cohortFlowsFakeRows) Err() error   { return nil }
func (r *cohortFlowsFakeRows) Close() error { return nil }

// cohortFlowsFakeConn fails the FIRST Query (the priced, joined SQL)
// with the ClickHouse "table does not exist" answer, and succeeds the
// second (the fallback SQL) — modelling a deployment where
// asset_month_usd_prices has not been applied.
type cohortFlowsFakeConn struct {
	driver.Conn
	calls []string
}

func (c *cohortFlowsFakeConn) Query(_ context.Context, query string, _ ...any) (driver.Rows, error) {
	c.calls = append(c.calls, query)
	if len(c.calls) == 1 {
		return nil, &clickhouse.Exception{
			Code: 60, Name: "UNKNOWN_TABLE",
			Message: "Table stellar.asset_month_usd_prices doesn't exist",
		}
	}
	return &cohortFlowsFakeRows{}, nil
}

// A missing asset_month_usd_prices table (GH-1078: the v0.91.0 cohort
// outage) must degrade the flows read — serve flows with no then-price —
// not fail the whole /graph/cohort route. Before the fix, readCohortFlows
// returned the UNKNOWN_TABLE error straight through and AccountCohort
// propagated it as a 500.
func TestCohortFlowsDegradeWhenPricesTableAbsent(t *testing.T) {
	conn := &cohortFlowsFakeConn{}
	r := &ExplorerReader{conn: conn}
	out := &AccountCohort{Relation: CohortRelationCreated, Root: "GTEST"}

	if err := r.readCohortFlows(context.Background(), out); err != nil {
		t.Fatalf("readCohortFlows returned an error on a missing optional table: %v", err)
	}
	if !out.FlowPricesUnavailable {
		t.Error("FlowPricesUnavailable was not set when asset_month_usd_prices is absent")
	}
	if len(out.Flows) != 1 {
		t.Fatalf("expected the fallback read to still serve the flow row, got %d rows", len(out.Flows))
	}
	if out.Flows[0].PriceUSDThen != nil {
		t.Errorf("PriceUSDThen should be nil in the fallback (no price join ran), got %v", *out.Flows[0].PriceUSDThen)
	}
	if len(conn.calls) != 2 {
		t.Fatalf("expected exactly 2 Query calls (priced, then fallback), got %d", len(conn.calls))
	}
}

// The holdings fold sums Int64 trustline balances: two members at 2^62
// in one asset must reach the Int128 column as 2^63, not wrapped.
func TestCohortHoldingsWidenBeforeSumming(t *testing.T) {
	for _, step := range cohortRollupStatements {
		if !strings.Contains(step.sql, "INSERT INTO stellar.account_cohort_holdings_staging") {
			continue
		}
		if !strings.Contains(step.sql, "sum(toInt128(e.balance))") {
			t.Errorf("holdings fold must widen each balance before summing:\n%s", step.sql)
		}
		return
	}
	t.Fatal("no holdings fold statement")
}
