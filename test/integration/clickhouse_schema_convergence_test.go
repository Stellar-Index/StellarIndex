//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// convergeDB is a scratch database so the check never mutates the shared
// `stellar` lake the other integration tests read.
const (
	convergeDB    = "stellar_converge"
	convergeProbe = convergeDB + ".converge_probe"
)

var (
	convergeStellarRef    = regexp.MustCompile(`\bstellar\.`)
	convergeCreateDB      = regexp.MustCompile(`(?i)^CREATE\s+DATABASE\s+IF\s+NOT\s+EXISTS\s+stellar$`)
	convergeAlterTable    = regexp.MustCompile(`(?is)^ALTER\s+TABLE\s+(\S+)\s+(.*)$`)
	convergeAddIfMissing  = regexp.MustCompile(`(?is)^ADD\s+(COLUMN|INDEX)\s+IF\s+NOT\s+EXISTS\s+(\w+)\s`)
	convergeUnconditional = regexp.MustCompile(`(?is)^(ADD\s+(COLUMN|INDEX)|DROP\s+INDEX|MODIFY\s+COLUMN)\s`)
)

// convergeSchema is table -> "column x" / "index y" -> full definition.
type convergeSchema map[string]map[string]string

// TestClickHouseAlterMigrationsConvergeWithFreshSchema pins that every
// ALTER-bearing deploy/clickhouse/*.sql artifact (the hand-applied upgrade
// path r1 took) lands on the same column and skip-index definitions that
// tier1_schema.sql's CREATE TABLE gives a fresh provision.
//
//   - Unconditional ALTERs (DROP+ADD INDEX) execute against the fresh schema,
//     so applying every artifact must change no tier-1 definition.
//   - `IF NOT EXISTS` ALTERs are no-ops there, which hides their definitions.
//     Each is replayed against a structural copy of its table with its
//     objects dropped, and must rebuild exactly the table's definitions.
//
// Ordinal column position is not compared: it depends on the order the files
// were run, and ch-schema-drift.sh checks it against the live lake.
func TestClickHouseAlterMigrationsConvergeWithFreshSchema(t *testing.T) {
	ctx := context.Background()
	conn := dialClickHouse(t, ctx, "default")
	dropConvergeDB(t, ctx, conn)
	t.Cleanup(func() { dropConvergeDB(t, context.Background(), conn) })

	convergeApply(t, ctx, conn, "tier1_schema.sql")
	fresh := convergeSnapshot(t, ctx, conn, "")

	files := convergeAlterFiles(t)
	if len(files) == 0 {
		t.Fatal("found no ALTER-bearing deploy/clickhouse/*.sql files; the glob or the filter is broken")
	}
	for _, f := range files {
		convergeApply(t, ctx, conn, f)
	}
	migrated := convergeSnapshot(t, ctx, conn, "")
	for table := range fresh {
		convergeDiff(t, "tier-1 table "+table+" after every ALTER artifact", fresh[table], migrated[table])
	}

	replayed := 0
	for _, f := range files {
		replayed += convergeReplayConditionalAdds(t, ctx, conn, f)
	}
	t.Logf("checked %d ALTER artifacts (%d IF NOT EXISTS statements replayed) against %d fresh tier-1 tables",
		len(files), replayed, len(fresh))
}

func dropConvergeDB(t *testing.T, ctx context.Context, conn driver.Conn) {
	t.Helper()
	if err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+convergeDB+" SYNC"); err != nil {
		t.Fatalf("drop %s: %v", convergeDB, err)
	}
}

// convergeStatements reads a deploy artifact and re-points it at convergeDB.
func convergeStatements(t *testing.T, name string) []string {
	t.Helper()
	stmts, err := clickHouseDeployStatements(name)
	if err != nil {
		t.Fatal(err)
	}
	for i, s := range stmts {
		if convergeCreateDB.MatchString(s) {
			stmts[i] = "CREATE DATABASE IF NOT EXISTS " + convergeDB
			continue
		}
		stmts[i] = convergeStellarRef.ReplaceAllString(s, convergeDB+".")
	}
	return stmts
}

func convergeApply(t *testing.T, ctx context.Context, conn driver.Conn, name string) {
	t.Helper()
	for _, s := range convergeStatements(t, name) {
		if err := conn.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %.96q: %v", name, s, err)
		}
	}
}

// convergeAlterFiles lists every deploy/clickhouse artifact other than the
// fresh schema that carries an ALTER TABLE statement, so a new one is covered
// without registering it here.
func convergeAlterFiles(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(clickHouseDeployDir(), "*.sql"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, p := range paths {
		name := filepath.Base(p)
		if name == "tier1_schema.sql" {
			continue
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range splitSQLStatements(string(raw)) {
			if convergeAlterTable.MatchString(s) {
				out = append(out, name)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// convergeReplayConditionalAdds replays each of name's all-`IF NOT EXISTS`
// ALTER statements against a copy of its table (`CREATE TABLE … AS`, so no
// materialized view pins the columns) from which the statement's objects were
// dropped, and requires the copy to come back identical to the table. It fails
// on any clause it does not model rather than leaving it unchecked.
func convergeReplayConditionalAdds(t *testing.T, ctx context.Context, conn driver.Conn, name string) int {
	t.Helper()
	replayed := 0
	for _, s := range convergeStatements(t, name) {
		m := convergeAlterTable.FindStringSubmatch(s)
		if m == nil {
			continue
		}
		strip := convergeStripStatements(t, name, m[2])
		if len(strip) == 0 {
			continue
		}
		convergeExec(t, ctx, conn, name, "DROP TABLE IF EXISTS "+convergeProbe+" SYNC")
		convergeExec(t, ctx, conn, name, "CREATE TABLE "+convergeProbe+" AS "+m[1])
		for _, st := range strip {
			convergeExec(t, ctx, conn, name, "ALTER TABLE "+convergeProbe+" "+st)
		}
		convergeExec(t, ctx, conn, name, "ALTER TABLE "+convergeProbe+" "+m[2])
		want := convergeSnapshot(t, ctx, conn, strings.TrimPrefix(m[1], convergeDB+"."))
		got := convergeSnapshot(t, ctx, conn, strings.TrimPrefix(convergeProbe, convergeDB+"."))
		convergeDiff(t, fmt.Sprintf("%s: %.60q replayed onto %s", name, s, m[1]),
			convergeOnly(want), convergeOnly(got))
		convergeExec(t, ctx, conn, name, "DROP TABLE IF EXISTS "+convergeProbe+" SYNC")
		replayed++
	}
	return replayed
}

// convergeStripStatements returns the DROP clauses that undo body's
// `ADD … IF NOT EXISTS` clauses, indexes first so a column's own index never
// blocks its drop. It returns nil for a statement of unconditional clauses
// (the fresh-schema pass already executed those) and fails on a mix.
func convergeStripStatements(t *testing.T, name, body string) []string {
	t.Helper()
	var indexes, columns []string
	unconditional := 0
	for _, clause := range convergeSplitClauses(body) {
		add := convergeAddIfMissing.FindStringSubmatch(clause + " ")
		switch {
		case add != nil && strings.EqualFold(add[1], "INDEX"):
			indexes = append(indexes, "DROP INDEX IF EXISTS "+add[2])
		case add != nil:
			columns = append(columns, "DROP COLUMN IF EXISTS "+add[2])
		case convergeUnconditional.MatchString(clause + " "):
			unconditional++
		default:
			t.Fatalf("%s: ALTER clause %.80q is not one this convergence check models; extend it", name, clause)
		}
	}
	if unconditional > 0 && len(indexes)+len(columns) > 0 {
		t.Fatalf("%s: ALTER mixes IF NOT EXISTS and unconditional clauses; split it so each half is checked", name)
	}
	return append(indexes, columns...)
}

// convergeSplitClauses splits an ALTER body on the commas between clauses,
// ignoring those nested inside parentheses (`Decimal(38, 7)`).
func convergeSplitClauses(body string) []string {
	var out []string
	depth, start := 0, 0
	for i, r := range body {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, strings.TrimSpace(body[start:i]))
				start = i + 1
			}
		}
	}
	return append(out, strings.TrimSpace(body[start:]))
}

func convergeExec(t *testing.T, ctx context.Context, conn driver.Conn, name, stmt string) {
	t.Helper()
	if err := conn.Exec(ctx, stmt); err != nil {
		t.Fatalf("%s: %.96q: %v", name, stmt, err)
	}
}

// convergeOnly returns the single table of a one-table snapshot.
func convergeOnly(s convergeSchema) map[string]string {
	for _, defs := range s {
		return defs
	}
	return nil
}

// convergeSnapshot reads the column and skip-index definitions of convergeDB,
// or of one table in it when table is non-empty.
func convergeSnapshot(t *testing.T, ctx context.Context, conn driver.Conn, table string) convergeSchema {
	t.Helper()
	out := convergeSchema{}
	put := func(tbl, key, def string) {
		if out[tbl] == nil {
			out[tbl] = map[string]string{}
		}
		out[tbl][key] = def
	}
	const filter = ` WHERE database = ? AND (? = '' OR table = ?)`
	cols, err := conn.Query(ctx, `SELECT table, name, type, default_kind, default_expression, compression_codec
		FROM system.columns`+filter, convergeDB, table, table)
	if err != nil {
		t.Fatalf("query system.columns: %v", err)
	}
	for cols.Next() {
		var tbl, name, typ, kind, expr, codec string
		if err := cols.Scan(&tbl, &name, &typ, &kind, &expr, &codec); err != nil {
			t.Fatalf("scan system.columns: %v", err)
		}
		put(tbl, "column "+name, strings.Join([]string{typ, kind, expr, codec}, " | "))
	}
	cols.Close()
	idx, err := conn.Query(ctx, `SELECT table, name, type_full, expr, granularity
		FROM system.data_skipping_indices`+filter, convergeDB, table, table)
	if err != nil {
		t.Fatalf("query system.data_skipping_indices: %v", err)
	}
	defer idx.Close()
	for idx.Next() {
		var tbl, name, typ, expr string
		var gran uint64
		if err := idx.Scan(&tbl, &name, &typ, &expr, &gran); err != nil {
			t.Fatalf("scan system.data_skipping_indices: %v", err)
		}
		put(tbl, "index "+name, fmt.Sprintf("%s | %s | GRANULARITY %d", typ, expr, gran))
	}
	if len(out) == 0 {
		t.Fatalf("snapshot of %s %q is empty; the schema did not apply", convergeDB, table)
	}
	return out
}

// convergeDiff reports every column or index whose definition differs.
func convergeDiff(t *testing.T, step string, want, got map[string]string) {
	t.Helper()
	keys := map[string]bool{}
	for k := range want {
		keys[k] = true
	}
	for k := range got {
		keys[k] = true
	}
	var drift []string
	for k := range keys {
		if w, g := want[k], got[k]; w != g {
			drift = append(drift, fmt.Sprintf("%s: fresh %q, via ALTER %q", k, w, g))
		}
	}
	sort.Strings(drift)
	if len(drift) > 0 {
		t.Errorf("%s: %d definition(s) differ:\n  %s", step, len(drift), strings.Join(drift, "\n  "))
	}
}
