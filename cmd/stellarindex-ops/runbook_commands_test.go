package main

import (
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	fencedBlockRE = regexp.MustCompile("(?s)```[^\n]*\n(.*?)```")
	inlineSpanRE  = regexp.MustCompile("`([^`]+)`")
	lineContRE    = regexp.MustCompile(`\\\n[ \t]*`)
	verbTokenRE   = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
)

// runbookCommandLines returns every shell line an operator could paste
// from a runbook: fenced blocks line by line (backslash continuations
// joined) and inline code spans, which markdown lets wrap across lines.
func runbookCommandLines(md string) []string {
	var out []string
	for _, m := range fencedBlockRE.FindAllStringSubmatch(md, -1) {
		out = append(out, strings.Split(lineContRE.ReplaceAllString(m[1], " "), "\n")...)
	}
	prose := fencedBlockRE.ReplaceAllString(md, "")
	for _, m := range inlineSpanRE.FindAllStringSubmatch(prose, -1) {
		if !strings.Contains(m[1], "\n\n") {
			out = append(out, strings.ReplaceAll(m[1], "\n", " "))
		}
	}
	return out
}

// opsInvocations yields the arguments following each stellarindex-ops
// binary token (bare, or by path) in line. A token carrying "=" is an
// env assignment naming the binary, not a call of it.
func opsInvocations(line string) [][]string {
	toks := strings.Fields(line)
	var out [][]string
	for i, tok := range toks {
		bin := strings.Trim(tok, `'"(`)
		if strings.Contains(bin, "=") || path.Base(bin) != "stellarindex-ops" {
			continue
		}
		var args []string
		for _, a := range toks[i+1:] {
			a = strings.Trim(a, `'"`)
			if a == "|" || a == ";" || a == "&&" || strings.HasPrefix(a, ">") {
				break
			}
			args = append(args, a)
		}
		out = append(out, args)
	}
	return out
}

func readRunbooks(t *testing.T) map[string]string {
	t.Helper()
	dir := filepath.Join(repoRootForOpsTest(t), "docs/operations/runbooks")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	out := map[string]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		out[e.Name()] = string(body)
	}
	if len(out) == 0 {
		t.Fatalf("no runbooks found under %s", dir)
	}
	return out
}

// TestRunbookOpsInvocationsNameRegisteredSubcommands: a runbook that tells
// the operator to run a stellarindex-ops verb the dispatcher does not
// register dies on "unknown command" mid-incident, often partway through a
// multi-step repair. `supply` verbs are nested, so its second word is
// checked against usageBody's `supply <verb>` entries.
func TestRunbookOpsInvocationsNameRegisteredSubcommands(t *testing.T) {
	checked := 0
	for name, md := range readRunbooks(t) {
		for _, line := range runbookCommandLines(md) {
			for _, args := range opsInvocations(line) {
				// A flag, placeholder or path after the binary (`--help`,
				// `<verb>`, `cp stellarindex-ops /dest`) is not a verb.
				if len(args) == 0 || !verbTokenRE.MatchString(args[0]) {
					continue
				}
				checked++
				verb := args[0]
				if _, ok := subcommands[verb]; !ok {
					t.Errorf("%s: `stellarindex-ops %s` — %q is not a registered subcommand", name, strings.Join(args, " "), verb)
					continue
				}
				if verb == "supply" && len(args) > 1 && !strings.Contains(usageBody, "\n  supply "+args[1]+" ") {
					t.Errorf("%s: `stellarindex-ops %s` — %q is not a supply subcommand", name, strings.Join(args, " "), args[1])
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("found no stellarindex-ops invocations in any runbook; the extractor is broken")
	}
}

var (
	createdRelationRE = regexp.MustCompile(`(?i)(?:CREATE\s+(?:UNLOGGED\s+)?(?:TABLE|(?:OR\s+REPLACE\s+)?(?:MATERIALIZED\s+)?VIEW)\s+(?:IF\s+NOT\s+EXISTS\s+)?(?:public\.)?|RENAME\s+TO\s+)"?([a-z_0-9]+)`)
	fromRelationRE    = regexp.MustCompile(`\bFROM\s+([a-z_][a-z_0-9.]*)`)
	cteNameRE         = regexp.MustCompile(`(?i)\b([a-z_0-9]+)\s+AS\s*\(`)
)

// TestRunbookSQLNamesMigratedRelations: the first diagnostic query of a
// page must not fail on a table that does not exist. Every unqualified
// relation a runbook SELECTs FROM must be created by some migration.
// Schema-qualified names (ClickHouse `stellar.*`, `timescaledb_information.*`),
// Postgres catalogues (`pg_*`), golang-migrate's own `schema_migrations`,
// CTE names and `…`-elided fragments are out of scope.
func TestRunbookSQLNamesMigratedRelations(t *testing.T) {
	root := repoRootForOpsTest(t)
	files, err := filepath.Glob(filepath.Join(root, "migrations", "*.up.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("glob migrations: %v (%d files)", err, len(files))
	}
	relations := map[string]bool{"schema_migrations": true}
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, m := range createdRelationRE.FindAllStringSubmatch(string(body), -1) {
			relations[strings.ToLower(m[1])] = true
		}
	}
	checked := 0
	for name, md := range readRunbooks(t) {
		for _, line := range runbookCommandLines(md) {
			if strings.Contains(line, "…") {
				continue
			}
			ctes := map[string]bool{}
			for _, m := range cteNameRE.FindAllStringSubmatch(line, -1) {
				ctes[strings.ToLower(m[1])] = true
			}
			for _, m := range fromRelationRE.FindAllStringSubmatch(line, -1) {
				rel := m[1]
				if strings.Contains(rel, ".") || strings.HasPrefix(rel, "pg_") || ctes[rel] {
					continue
				}
				checked++
				if !relations[rel] {
					t.Errorf("%s: %q — no migration creates a relation named %q", name, line, rel)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("found no SQL FROM clause in any runbook; the extractor is broken")
	}
}

// TestRunbooksCiteRealIdentifiers pins identifiers runbooks have used
// that do not exist, each alongside what does.
func TestRunbooksCiteRealIdentifiers(t *testing.T) {
	forbidden := []struct {
		re   *regexp.Regexp
		real string
	}{
		{regexp.MustCompile(`redis_url`), "there is no redis_url key: [storage] redis_addr, password via STELLARINDEX_REDIS_PASSWORD"},
		{regexp.MustCompile(`ch-rebuild\b[^\n]*?\s-source\b`), "ch-rebuild's flag is -sources (plural)"},
		{regexp.MustCompile(`^soroswap-skim$`), "no projector source is named soroswap-skim; skim rows replay under soroswap"},
	}
	for name, md := range readRunbooks(t) {
		for _, line := range runbookCommandLines(md) {
			for _, f := range forbidden {
				if f.re.MatchString(line) {
					t.Errorf("%s: %q — %s", name, line, f.real)
				}
			}
		}
	}
}
