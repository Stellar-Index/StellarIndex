package clickhouse

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// castOfSum matches a widening cast applied to an aggregate's RESULT.
// ClickHouse's sum() over an Int64 column returns Int64 and wraps on
// overflow, so a cast outside the aggregate widens an already-wrapped
// total; the cast has to go on the argument, as asset_supply_reader.go's
// sum(toInt128(balance)) does.
var castOfSum = regexp.MustCompile(`to(U)?Int(128|256)\(\s*sum(If)?\(`)

// TestNoWideningCastAppliedToASumResult scans every lake query in the
// package, so a new rollup cannot reintroduce the wrapped-then-widened
// shape even over a column that is Int64 today.
func TestNoWideningCastAppliedToASumResult(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob package sources: %v", err)
	}
	scanned := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		scanned++
		for i, line := range strings.Split(string(src), "\n") {
			if castOfSum.MatchString(line) {
				t.Errorf("%s:%d widens a sum's result, not its argument (Int64 sum wraps first): %s",
					f, i+1, strings.TrimSpace(line))
			}
		}
	}
	if scanned == 0 {
		t.Fatal("scanned no package sources; the guard did not run")
	}
}

// The holdings step sums Int64 ledger_entries_current.balance per
// (cohort, asset); a trustline asset's cohort total can exceed int64.
func TestCohortHoldingsWidenBalanceBeforeSumming(t *testing.T) {
	for _, s := range cohortRollupStatements {
		if strings.Contains(s.sql, "INSERT INTO stellar.account_cohort_holdings_staging") {
			if !strings.Contains(s.sql, "sum(toInt128(e.balance)) AS balance") {
				t.Fatalf("holdings step does not widen balance inside sum():\n%s", s.sql)
			}
			return
		}
	}
	t.Fatal("no account_cohort_holdings_staging step found")
}

// live_balance is an Int64 account balance; the creators board widens it
// before the per-creator sum.
func TestCreatorsBoardWidensLiveBalanceBeforeSumming(t *testing.T) {
	for _, s := range creatorsBoardSteps() {
		if strings.Contains(s.sql, "INSERT INTO stellar.account_creators_rollup_staging") {
			if !strings.Contains(s.sql, "sum(toInt128(live_balance)) AS live_stroops") {
				t.Fatalf("creators board does not widen live_balance inside sum():\n%s", s.sql)
			}
			return
		}
	}
	t.Fatal("no account_creators_rollup_staging step found")
}
