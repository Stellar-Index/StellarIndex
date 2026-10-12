package clickhouse

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// censusDDL returns the comment-stripped body of one CREATE statement from
// deploy/clickhouse/tier1_schema.sql (the fresh-host DDL every mirror is
// gated against), or "" when the file declares no such object.
func censusDDL(t *testing.T, object string) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
	raw, err := os.ReadFile(filepath.Join(root, "deploy", "clickhouse", "tier1_schema.sql"))
	if err != nil {
		t.Fatalf("read tier1_schema.sql: %v", err)
	}
	var kept []string
	for _, line := range strings.Split(string(raw), "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		kept = append(kept, line)
	}
	text := strings.Join(kept, "\n")
	start := strings.Index(text, "CREATE TABLE IF NOT EXISTS "+object+"\n")
	if start < 0 {
		return ""
	}
	end := strings.Index(text[start:], ";")
	if end < 0 {
		t.Fatalf("tier1_schema.sql: unterminated CREATE for %s", object)
	}
	return text[start : start+end]
}

// TestCensusDayFilterHasASkipIndex pins the census rollup's close_time predicate to a declared minmax skip
// index: contract_events is partitioned and ordered by ledger_seq, so without the index the filter prunes
// nothing and every run reads every granule. The predicate column and the index may change, but not apart.
func TestCensusDayFilterHasASkipIndex(t *testing.T) {
	where := regexp.MustCompile(`WHERE\s+(\w+)\s*>=\s*\?\s+AND\s+(\w+)\s*<\s*\?`).
		FindStringSubmatch(censusDayInsert("x"))
	if where == nil || where[1] != where[2] {
		t.Fatalf("censusDayInsert: want a single-column half-open day window, got %q", censusDayInsert("x"))
	}
	col := where[1]

	ddl := censusDDL(t, "stellar.contract_events")
	if ddl == "" {
		t.Fatal("tier1_schema.sql does not declare stellar.contract_events")
	}
	idx := regexp.MustCompile(`INDEX\s+\w+\s+` + col + `\s+TYPE\s+minmax\s+GRANULARITY\s+\d+`)
	if !idx.MatchString(ddl) {
		t.Fatalf("stellar.contract_events declares no minmax skip index on %s, the column censusDayInsert filters on — the day window full-scans the table:\n%s", col, ddl)
	}
}

// TestCensusStagingTableIsPerRunOnly pins that no DDL file declares a shared
// stellar.contracts_census_daily_staging. RunCensusDay CREATEs a randomly suffixed private staging
// table per run and DROPs it, so a static twin is dead DDL that reads as a second writer path.
func TestCensusStagingTableIsPerRunOnly(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
	for _, rel := range []string{"tier1_schema.sql", "contracts_census_daily.sql"} {
		raw, err := os.ReadFile(filepath.Join(root, "deploy", "clickhouse", rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if regexp.MustCompile(`(?i)CREATE\s+TABLE\s+(IF\s+NOT\s+EXISTS\s+)?stellar\.contracts_census_daily_staging\b`).Match(raw) {
			t.Errorf("%s declares stellar.contracts_census_daily_staging, which no code path writes: the rollup uses a private per-run table", rel)
		}
	}
	name, err := privateStagingTable()
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^contracts_census_daily_staging_[0-9a-f]{16}$`).MatchString(name) {
		t.Fatalf("privateStagingTable() = %q, want contracts_census_daily_staging_<16 hex>", name)
	}
}

// TestCensusDayInsertNamesItsColumns pins an explicit target column list in the order contracts_census_daily
// declares, so a column added on either side fails loudly instead of silently swapping values.
func TestCensusDayInsertNamesItsColumns(t *testing.T) {
	stmt := censusDayInsert("contracts_census_daily_staging_test")
	want := `INSERT INTO stellar.contracts_census_daily_staging_test (day, contract_id, events, last_ledger, last_seen)`
	if !strings.Contains(strings.Join(strings.Fields(stmt), " "), want) {
		t.Fatalf("censusDayInsert does not name its target columns in the declared table order:\n got: %s\nwant substring: %s",
			strings.Join(strings.Fields(stmt), " "), want)
	}
}
