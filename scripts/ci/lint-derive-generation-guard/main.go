// lint-derive-generation-guard enforces INV-3 (migrations 0109/0110): every
// hand-written `INSERT INTO <table> ... ON CONFLICT ... DO UPDATE` writer for
// a table that carries a `derive_generation` column MUST guard the update
// with `WHERE <table>.derive_generation <= EXCLUDED.derive_generation`, so a
// corrected re-derive lands in place and a stale lower-generation replay can
// never revert it. Without the guard, `ON CONFLICT DO NOTHING` (or a DO
// UPDATE missing the WHERE) silently no-ops a correction — the only way out
// is a destructive DELETE + full re-backfill.
//
// Until now this was enforced only by copy-paste convention across ~39
// files (T351): nothing rejected a 40th writer that forgot the WHERE clause.
// This is the static PR-time complement to the two representative-table
// integration test (test/integration/derive_generation_guard_protocol_test.go).
//
// Writers that build the INSERT dynamically (table name interpolated via
// fmt.Sprintf rather than written as a literal) are exempt: the guard clause
// is baked into their single shared builder rather than repeated per table,
// so a literal `INSERT INTO <table>` won't be found and the lint has nothing
// to check — see copyMergeUpsertSQL in internal/storage/timescale/sep41_copy.go.
//
// Usage: go run ./scripts/ci/lint-derive-generation-guard
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// allow maps a table to the reason a literal ON CONFLICT DO UPDATE writer
// for it is exempt from the guard requirement (e.g. a genuinely append-only
// table whose derive_generation column exists for a different purpose).
// Empty today — every current writer complies — but kept as the place a
// future justified exception is recorded, WITH A REASON, mirroring
// lint-pk-discriminators' allow map.
var allow = map[string]string{}

var (
	lineComment  = regexp.MustCompile(`--[^\n]*`)
	blockComment = regexp.MustCompile(`(?s)/\*.*?\*/`)
	alterAddCol  = regexp.MustCompile(`(?i)ALTER TABLE\s+(?:ONLY\s+)?([a-z0-9_]+)\s+ADD COLUMN\s+derive_generation\b`)
	createRe     = regexp.MustCompile(`(?i)CREATE TABLE\s+(?:IF NOT EXISTS\s+)?([a-z0-9_]+)\s*\(`)
	createColRe  = regexp.MustCompile(`(?i)\bderive_generation\s+(bigint|integer)\b`)
	rawLitRe     = regexp.MustCompile("(?s)`([^`]*)`")
	insertTblRe  = regexp.MustCompile(`(?i)INSERT INTO\s+([a-z0-9_]+)`)
)

// deriveGenerationTables returns the set of tables that carry a
// derive_generation column, parsed from migrations/*.up.sql.
func deriveGenerationTables(migrationsDir string) (map[string]bool, error) {
	entries, err := filepath.Glob(filepath.Join(migrationsDir, "*.up.sql"))
	if err != nil {
		return nil, err
	}
	sort.Strings(entries)

	tables := map[string]bool{}
	for _, f := range entries {
		b, rerr := os.ReadFile(f) //nolint:gosec // f comes from a fixed Glob of migrations/, not user input
		if rerr != nil {
			return nil, fmt.Errorf("read %s: %w", f, rerr)
		}
		content := blockComment.ReplaceAllString(string(b), "")
		content = lineComment.ReplaceAllString(content, "")

		for _, m := range alterAddCol.FindAllStringSubmatch(content, -1) {
			tables[strings.ToLower(m[1])] = true
		}
		// CREATE TABLE bodies: a table is a match if its CREATE statement
		// (from the CREATE TABLE keyword to the next CREATE TABLE / EOF)
		// declares a derive_generation column inline.
		locs := createRe.FindAllStringSubmatchIndex(content, -1)
		for i, loc := range locs {
			end := len(content)
			if i+1 < len(locs) {
				end = locs[i+1][0]
			}
			body := content[loc[0]:end]
			if createColRe.MatchString(body) {
				name := content[loc[2]:loc[3]]
				tables[strings.ToLower(name)] = true
			}
		}
	}
	return tables, nil
}

// checkGoSource scans one Go file's content for raw-string-literal SQL
// blocks that literally INSERT INTO a derive_generation-carrying table with
// ON CONFLICT ... DO UPDATE, and returns one failure message per literal
// missing the generation guard.
func checkGoSource(path, content string, tables map[string]bool) []string {
	var failures []string
	for _, lit := range rawLitRe.FindAllString(content, -1) {
		if !strings.Contains(lit, "ON CONFLICT") || !strings.Contains(lit, "DO UPDATE") {
			continue
		}
		m := insertTblRe.FindStringSubmatch(lit)
		if m == nil {
			continue
		}
		table := strings.ToLower(m[1])
		if !tables[table] {
			continue
		}
		if _, ok := allow[table]; ok {
			continue
		}
		guard := table + ".derive_generation <= EXCLUDED.derive_generation"
		if !strings.Contains(lit, guard) {
			failures = append(failures, fmt.Sprintf("%s: INSERT INTO %s ... ON CONFLICT DO UPDATE lacks `WHERE %s`", path, table, guard))
		}
	}
	return failures
}

func main() {
	tables, err := deriveGenerationTables("migrations")
	if err != nil {
		fmt.Fprintf(os.Stderr, "lint-derive-generation-guard: %v\n", err)
		os.Exit(2)
	}
	if len(tables) == 0 {
		fmt.Fprintln(os.Stderr, "lint-derive-generation-guard: no derive_generation tables found in migrations/ (typo in the parse?)")
		os.Exit(2)
	}

	entries, err := filepath.Glob(filepath.Join("internal", "storage", "timescale", "*.go"))
	if err != nil || len(entries) == 0 {
		fmt.Fprintln(os.Stderr, "lint-derive-generation-guard: no files found under internal/storage/timescale/")
		os.Exit(2)
	}
	sort.Strings(entries)

	var failures []string
	checked := 0
	for _, f := range entries {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, rerr := os.ReadFile(f) //nolint:gosec // f comes from a fixed Glob of internal/storage/timescale/, not user input
		if rerr != nil {
			fmt.Fprintf(os.Stderr, "lint-derive-generation-guard: read %s: %v\n", f, rerr)
			os.Exit(2)
		}
		checked++
		failures = append(failures, checkGoSource(f, string(b), tables)...)
	}

	if len(failures) > 0 {
		fmt.Fprintln(os.Stderr, "lint-derive-generation-guard: FAIL — writers missing the INV-3 generation guard:")
		for _, f := range failures {
			fmt.Fprintln(os.Stderr, "  ✗", f)
		}
		fmt.Fprintln(os.Stderr, "\nSee migrations/0109_derive_generation_guard.up.sql. Add `WHERE <table>.derive_generation <= EXCLUDED.derive_generation`")
		fmt.Fprintln(os.Stderr, "to the ON CONFLICT DO UPDATE, or add a reasoned exemption to `allow` in this lint.")
		os.Exit(1)
	}
	fmt.Printf("lint-derive-generation-guard: OK — %d files checked, %d derive_generation tables tracked, all literal upserts guarded\n", checked, len(tables))
}
