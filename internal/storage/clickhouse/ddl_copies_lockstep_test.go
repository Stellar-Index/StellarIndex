package clickhouse

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// GH-1169: supplyFlowsDDL, accountMovementsDDL and
// scripts/ops/d3-lecur-v2-rebuild.sh's `setup` phase are hand-maintained
// copies of DDL that scripts/ci/lint-ch-apply-scope.sh and
// configs/ansible/roles/archival-node/files/ch-schema-drift.sh never see —
// the first only scans deploy/clickhouse/*.sql, the second only compares a
// live host to tier1_schema.sql. Nothing catches the three drifting from
// their canonical twin. This pins all three, byte-for-byte modulo comments
// and whitespace, the way TestTTLLiveUntilDDLIsShapeGuarded pins the TTL
// extraction.

// lockstepRepoRoot resolves the module root relative to this test file.
func lockstepRepoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
}

func lockstepReadFile(t *testing.T, root, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(raw)
}

// balancedParenEnd returns the index of the ')' that closes the '(' at
// start, honoring nesting (JSONExtractString(...), bloom_filter(...) etc.
// both appear inside the column lists this test compares).
func balancedParenEnd(t *testing.T, s string, start int) int {
	t.Helper()
	depth := 0
	for i := start; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	t.Fatalf("unbalanced parens starting at byte %d", start)
	return -1
}

// extractCreateTable pulls one `CREATE TABLE ... stellar.<table> ( ... )
// ENGINE ... ORDER BY (...)` statement out of a larger blob — a Go string
// literal, a shell heredoc/quoted string, or a multi-statement .sql file —
// stopping at the closing paren of ORDER BY's column tuple. None of the
// four source shapes this test reads reliably terminate a statement with a
// character (Go/shell have none; .sql uses `;` but so do inline runbook
// comments), so the ORDER BY clause every one of these DDLs declares is the
// one anchor common to all of them.
func extractCreateTable(t *testing.T, src, table string) string {
	t.Helper()
	anchor := regexp.MustCompile(`CREATE TABLE(?:\s+IF NOT EXISTS)?\s+stellar\.` + regexp.QuoteMeta(table) + `\s*\(`)
	loc := anchor.FindStringIndex(src)
	if loc == nil {
		t.Fatalf("no CREATE TABLE for stellar.%s found", table)
	}
	stmt := src[loc[0]:]
	colParenStart := strings.Index(stmt, "(")
	colParenEnd := balancedParenEnd(t, stmt, colParenStart)

	afterCols := stmt[colParenEnd:]
	obIdx := strings.Index(afterCols, "ORDER BY (")
	if obIdx < 0 {
		t.Fatalf("no ORDER BY (...) clause for stellar.%s", table)
	}
	obParenStart := colParenEnd + obIdx + strings.Index(afterCols[obIdx:], "(")
	obParenEnd := balancedParenEnd(t, stmt, obParenStart)
	return stmt[:obParenEnd+1]
}

// normalizeDDLStatement strips `--` line comments and collapses whitespace
// so two DDL copies that differ only in formatting or commentary compare
// equal; a real drift in a column, type, engine or key still fails.
func normalizeDDLStatement(stmt string) string {
	var kept []string
	for _, line := range strings.Split(stmt, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(strings.Fields(strings.Join(kept, " ")), " ")
}

func assertDDLLockstep(t *testing.T, table, gotLabel, got, wantLabel, want string) {
	t.Helper()
	gotNorm := normalizeDDLStatement(extractCreateTable(t, got, table))
	wantNorm := normalizeDDLStatement(extractCreateTable(t, want, table))
	if gotNorm != wantNorm {
		t.Errorf("stellar.%s drifted between %s and %s (canonical):\n%s: %s\n%s: %s",
			table, gotLabel, wantLabel, gotLabel, gotNorm, wantLabel, wantNorm)
	}
}

// TestSupplyFlowsDDLMatchesTier1Schema pins internal/storage/clickhouse's
// supplyFlowsDDL to deploy/clickhouse/tier1_schema.sql, the file
// EnsureSupplyFlowsTable's own doc comment claims it is "kept in sync
// with". Whichever one a host applies first wins on IF NOT EXISTS,
// so the two must be identical, not merely both plausible.
func TestSupplyFlowsDDLMatchesTier1Schema(t *testing.T) {
	root := lockstepRepoRoot(t)
	tier1 := lockstepReadFile(t, root, filepath.Join("deploy", "clickhouse", "tier1_schema.sql"))
	assertDDLLockstep(t, "supply_flows",
		"internal/storage/clickhouse/supply_flows.go:supplyFlowsDDL", supplyFlowsDDL,
		"deploy/clickhouse/tier1_schema.sql", tier1)
}

// TestAccountMovementsDDLMatchesTier1Schema is the account_movements
// twin of the test above.
func TestAccountMovementsDDLMatchesTier1Schema(t *testing.T) {
	root := lockstepRepoRoot(t)
	tier1 := lockstepReadFile(t, root, filepath.Join("deploy", "clickhouse", "tier1_schema.sql"))
	assertDDLLockstep(t, "account_movements",
		"internal/storage/clickhouse/account_movements.go:accountMovementsDDL", accountMovementsDDL,
		"deploy/clickhouse/tier1_schema.sql", tier1)
}

// TestLecurV2RebuildScriptMatchesOperatorDDL pins scripts/ops/d3-lecur-v2-rebuild.sh's
// `setup` phase CREATE TABLE to deploy/clickhouse/ledger_entries_current_intra_ledger_seq.sql,
// the operator artifact it is meant to reproduce interactively. A drift here
// means the scripted cutover and the documented runbook build two different
// tables under the same name.
func TestLecurV2RebuildScriptMatchesOperatorDDL(t *testing.T) {
	root := lockstepRepoRoot(t)
	script := lockstepReadFile(t, root, filepath.Join("scripts", "ops", "d3-lecur-v2-rebuild.sh"))
	operator := lockstepReadFile(t, root, filepath.Join("deploy", "clickhouse", "ledger_entries_current_intra_ledger_seq.sql"))
	assertDDLLockstep(t, "ledger_entries_current_v2",
		"scripts/ops/d3-lecur-v2-rebuild.sh", script,
		"deploy/clickhouse/ledger_entries_current_intra_ledger_seq.sql", operator)
}

// TestD2OrdinalReprojectInsertNamesItsColumns pins d2-ordinal-reproject.sh's
// staging INSERT to an EXPLICIT target column list. $STAGE is `CREATE TABLE
// $STAGE AS stellar.ledger_entry_changes`, so a positional INSERT depends on
// intra_ledger_seq staying the LAST column of stellar.ledger_entry_changes —
// true only because tier1_schema.sql:216-222 documents it was appended by
// ALTER TABLE. The next ADD COLUMN, or the documented `DROP COLUMN IF EXISTS
// intra_ledger_seq` rollback, shifts it and a positional INSERT would land a
// value in the wrong column with no query error (GH-1169).
func TestD2OrdinalReprojectInsertNamesItsColumns(t *testing.T) {
	root := lockstepRepoRoot(t)
	script := lockstepReadFile(t, root, filepath.Join("scripts", "ops", "d2-ordinal-reproject.sh"))
	want := regexp.MustCompile(`INSERT INTO \$STAGE\s*\([^)]*intra_ledger_seq[^)]*\)`)
	if !want.MatchString(script) {
		t.Fatal("scripts/ops/d2-ordinal-reproject.sh: INSERT INTO $STAGE does not name its target columns (including intra_ledger_seq) — it relies on positional order matching stellar.ledger_entry_changes's current column layout")
	}
}
