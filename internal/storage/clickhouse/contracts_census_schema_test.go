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

// TestCensusDayFilterHasASkipIndex pins the census rollup's day predicate to
// a declared minmax skip index on the same column. contract_events is
// PARTITION BY intDiv(ledger_seq, …) and ORDER BY (ledger_seq, …), and
// ClickHouse keeps part-level min/max only for partition-key columns — so a
// WHERE on close_time prunes NOTHING unless the schema declares a skip
// index for it. Before the index existed, the 30-minute rollup read every
// granule of the billions-row table per run while its own comment claimed
// "ClickHouse prunes contract_events parts by its close_time minmax index".
// Either side may change (the predicate column, the index), but not apart.
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

// TestCensusStagingTableIsPerRunOnly pins that no DDL file declares a
// shared stellar.contracts_census_daily_staging. RunCensusDay CREATEs a
// crypto-random-suffixed private staging table per run and DROPs it (the
// W1-chrollup-4 isolation), so a static twin is dead DDL that reads as a
// second writer path and shows up on every host as an empty orphan.
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
