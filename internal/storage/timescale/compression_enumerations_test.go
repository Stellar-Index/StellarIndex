package timescale

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The compression-policy want-list is written down in TWO operator files
// that must agree with each other and with the schema:
//
//   - scripts/ops/add-missing-compression-policies.sql — the DO block that
//     ATTACHES a policy to each compression-eligible hypertable. It runs
//     under `\set ON_ERROR_STOP on`, so `add_compression_policy` against a
//     table that no longer exists raises and aborts the whole block: every
//     table listed AFTER the missing one silently never gets its policy.
//   - scripts/ops/config-assertions.sh — `compression_policies_applied`,
//     which COUNTS want-list tables with no policy job and pages (via
//     stellarindex_config_assertion_failed) when the count is non-zero. A
//     dropped table has no job, so it fails that assertion forever.
//
// Neither failure is visible from Go: one is a psql script an operator runs
// by hand post-Phase-D, the other is a shell assertion on r1. Migration 0152
// (#358) dropped three tables that were in both lists — tvl_observations,
// classic_asset_stats_5m and aggregator_exposures — and this test is what
// makes the next such drop a red build instead of a silent operational
// regression.
//
// It asserts three things:
//
//  1. The two lists are IDENTICAL. They are the same claim written twice;
//     a table in the assertion but not the script pages for a policy
//     nothing will ever attach.
//  2. Every table in them is CREATEd by a migration.
//  3. None of them is DROPped by a LATER migration.
//
// Deliberately NOT asserted: that each listed table is compression-eligible
// (`ALTER TABLE … SET (timescaledb.compress …)`). That is a real property,
// but several of these set it inside a `DO` block or via a helper, and a
// half-right parse that passes on a table it cannot see would be worse than
// no check. The three assertions above are the ones that were actually
// violated.

var (
	// A quoted table name inside a SQL ARRAY[...] literal.
	sqlArrayItemRe = regexp.MustCompile(`'([a-z][a-z0-9_]*)'`)

	// `CREATE TABLE [IF NOT EXISTS] name` at the start of a statement.
	createTableRe = regexp.MustCompile(`(?m)^\s*CREATE\s+TABLE(?:\s+IF\s+NOT\s+EXISTS)?\s+([a-z][a-z0-9_]*)`)

	// `DROP TABLE [IF EXISTS] name` at the start of a statement.
	dropTableRe = regexp.MustCompile(`(?m)^\s*DROP\s+TABLE(?:\s+IF\s+EXISTS)?\s+([a-z][a-z0-9_]*)`)
)

// sliceBetween returns the text of s between the first occurrence of start
// and the first occurrence of end after it. Fails the test if either marker
// is missing, so a refactor of the operator file turns this check red
// instead of vacuous.
func sliceBetween(t *testing.T, s, start, end, what string) string {
	t.Helper()
	i := strings.Index(s, start)
	if i < 0 {
		t.Fatalf("%s: marker %q not found — the file's shape changed and this "+
			"parse has gone vacuous. Fix the marker, do not delete the test.", what, start)
	}
	rest := s[i+len(start):]
	j := strings.Index(rest, end)
	if j < 0 {
		t.Fatalf("%s: end marker %q not found after %q — see above.", what, end, start)
	}
	return rest[:j]
}

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(findRepoRoot(t), rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// stripSQLComments drops whole-line `--` comments so a table name mentioned
// in prose (e.g. the "REMOVED with migration 0152" note at the top of the
// policy script) is never mistaken for a list entry.
func stripSQLComments(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

func compressionPolicyScriptTables(t *testing.T) []string {
	t.Helper()
	body := readRepoFile(t, "scripts/ops/add-missing-compression-policies.sql")
	block := sliceBetween(t, body, "tables text[] := ARRAY[", "];",
		"add-missing-compression-policies.sql")
	return uniqueSorted(matchAll(sqlArrayItemRe, stripSQLComments(block)))
}

func configAssertionTables(t *testing.T) []string {
	t.Helper()
	body := readRepoFile(t, "scripts/ops/config-assertions.sh")
	block := sliceBetween(t, body, "SELECT count(*) FROM unnest(ARRAY[", "]) AS want(tbl)",
		"config-assertions.sh compression_policies_applied")
	return uniqueSorted(matchAll(sqlArrayItemRe, stripSQLComments(block)))
}

func matchAll(re *regexp.Regexp, s string) []string {
	var out []string
	for _, m := range re.FindAllStringSubmatch(s, -1) {
		out = append(out, m[1])
	}
	return out
}

func uniqueSorted(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// tableLifecycle walks migrations/*.up.sql in NUMBER order and records, per
// table, whether the last statement touching it was a CREATE (live) or a
// DROP (gone).
func tableLifecycle(t *testing.T) map[string]bool {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(findRepoRoot(t), "migrations", "*.up.sql"))
	if err != nil {
		t.Fatalf("glob migrations: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("no migrations found — repo root resolution must be broken")
	}
	sort.Strings(paths) // NNNN_ prefix makes lexical order == numeric order

	live := make(map[string]bool)
	for _, p := range paths {
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			t.Fatalf("read %s: %v", p, rerr)
		}
		body := stripSQLComments(string(b))
		for _, name := range matchAll(createTableRe, body) {
			live[name] = true
		}
		for _, name := range matchAll(dropTableRe, body) {
			live[name] = false
		}
	}
	return live
}

func TestCompressionPolicyEnumerationsTrackTheSchema(t *testing.T) {
	t.Parallel()

	script := compressionPolicyScriptTables(t)
	assertion := configAssertionTables(t)

	if len(script) == 0 || len(assertion) == 0 {
		t.Fatalf("parsed %d script tables and %d assertion tables — a zero on a "+
			"non-empty file means the parse broke, not that the lists are empty",
			len(script), len(assertion))
	}
	t.Logf("parsed %d tables from add-missing-compression-policies.sql and %d from "+
		"config-assertions.sh", len(script), len(assertion))

	if strings.Join(script, ",") != strings.Join(assertion, ",") {
		t.Errorf("the compression want-list disagrees between the two operator files.\n"+
			"  add-missing-compression-policies.sql: %v\n"+
			"  config-assertions.sh:                 %v\n"+
			"They are the same claim written twice — a table in the assertion but not "+
			"the script pages forever for a policy nothing will attach.", script, assertion)
	}

	live := tableLifecycle(t)
	for _, tbl := range script {
		switch created, ok := live[tbl]; {
		case !ok:
			t.Errorf("add-missing-compression-policies.sql lists %q but NO migration "+
				"creates it. `\\set ON_ERROR_STOP on` + add_compression_policy on a "+
				"missing table aborts the whole DO block, so every table listed after "+
				"it never gets a policy.", tbl)
		case !created:
			t.Errorf("add-missing-compression-policies.sql lists %q but a migration "+
				"DROPS it. Remove it from BOTH that script and "+
				"config-assertions.sh's compression_policies_applied want-list in the "+
				"same change as the drop: the script aborts mid-list, and the "+
				"assertion fails forever on a table that is gone on purpose.", tbl)
		}
	}
}
