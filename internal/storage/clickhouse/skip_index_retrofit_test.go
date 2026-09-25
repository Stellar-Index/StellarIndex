package clickhouse

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// GH-1160: a skip index declared only inside tier1_schema.sql's
// `CREATE TABLE IF NOT EXISTS` never reaches a host whose table predates it —
// the CREATE is a no-op there, and `MATERIALIZE INDEX` fails on an index the
// table does not have. Every tier1 skip index therefore needs an
// `ALTER TABLE … ADD INDEX` beside it in an operator artifact, with the same
// definition, so the documented retrofit is executable.

var (
	tier1CreateTableRE = regexp.MustCompile(`^CREATE TABLE (?:IF NOT EXISTS )?stellar\.(\w+) \(`)
	tier1IndexRE       = regexp.MustCompile(`[,(] ?INDEX (\w+) (.+? GRANULARITY \d+)`)
	alterTableRE       = regexp.MustCompile(`^ALTER TABLE stellar\.(\w+) `)
	addIndexRE         = regexp.MustCompile(`ADD INDEX (?:IF NOT EXISTS )?(\w+) (.+? GRANULARITY \d+)`)
)

// ddlStatements strips comments, collapses whitespace and splits on `;`.
func ddlStatements(src string) []string {
	var out []string
	for _, s := range strings.Split(normalizeDDLStatement(src), ";") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// skipIndexDefs maps "<table>.<index>" to every definition the statements
// matching stmtRE declare for it, as matched by idxRE.
func skipIndexDefs(src string, stmtRE, idxRE *regexp.Regexp, into map[string][]string) {
	for _, stmt := range ddlStatements(src) {
		m := stmtRE.FindStringSubmatch(stmt)
		if m == nil {
			continue
		}
		for _, im := range idxRE.FindAllStringSubmatch(stmt, -1) {
			key := m[1] + "." + im[1]
			into[key] = append(into[key], im[2])
		}
	}
}

func TestEveryTier1SkipIndexHasAnAddIndexRetrofit(t *testing.T) {
	root := lockstepRepoRoot(t)
	dir := filepath.Join(root, "deploy", "clickhouse")

	declared := map[string][]string{}
	skipIndexDefs(lockstepReadFile(t, root, filepath.Join("deploy", "clickhouse", "tier1_schema.sql")),
		tier1CreateTableRE, tier1IndexRE, declared)
	if len(declared) == 0 {
		t.Fatal("parsed zero INDEX declarations from tier1_schema.sql; the parser, not the schema, is broken")
	}

	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		t.Fatal(err)
	}
	retrofits := map[string][]string{}
	for _, f := range files {
		if filepath.Base(f) == "tier1_schema.sql" {
			continue
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		skipIndexDefs(string(raw), alterTableRE, addIndexRE, retrofits)
	}

	for key, defs := range declared {
		if len(defs) != 1 {
			t.Errorf("tier1_schema.sql declares %s %d times: %q", key, len(defs), defs)
			continue
		}
		have := retrofits[key]
		found := false
		for _, d := range have {
			found = found || d == defs[0]
		}
		if !found {
			t.Errorf("stellar.%s (%s) has no matching `ALTER TABLE … ADD INDEX` in any "+
				"deploy/clickhouse operator artifact (found %q); an existing host cannot acquire it",
				key, defs[0], have)
		}
	}
	t.Logf("checked %d tier1 skip indexes against %d operator artifacts", len(declared), len(files)-1)
}
