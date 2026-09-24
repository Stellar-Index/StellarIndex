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
	migrateFencedRE  = regexp.MustCompile("(?s)```[^\n]*\n(.*?)```")
	migrateInlineRE  = regexp.MustCompile("`([^`]+)`")
	migrateLineContR = regexp.MustCompile(`\\\n[ \t]*`)
)

// TestRunbookMigrateInvocationsUseDefinedFlags: an unknown flag exits 2, so
// a runbook's manual migrate step (the recovery for a missing table) fails
// at the point of use unless every flag it passes is one newFlagSet declares.
func TestRunbookMigrateInvocationsUseDefinedFlags(t *testing.T) {
	fs, _, _, _, _ := newFlagSet()
	dir := filepath.Join("..", "..", "docs", "operations", "runbooks")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	checked := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		for _, line := range migrateCommandLines(string(body)) {
			for _, flagName := range migrateInvocationFlags(line) {
				checked++
				if flagName != "h" && flagName != "help" && fs.Lookup(flagName) == nil {
					t.Errorf("%s: `%s` — stellarindex-migrate has no -%s flag", e.Name(), strings.TrimSpace(line), flagName)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("found no flagged stellarindex-migrate invocation in any runbook; the extractor is broken")
	}
}

// migrateCommandLines returns fenced-block lines (continuations joined) and
// inline code spans, which markdown lets wrap across lines.
func migrateCommandLines(md string) []string {
	var out []string
	for _, m := range migrateFencedRE.FindAllStringSubmatch(md, -1) {
		out = append(out, strings.Split(migrateLineContR.ReplaceAllString(m[1], " "), "\n")...)
	}
	for _, m := range migrateInlineRE.FindAllStringSubmatch(migrateFencedRE.ReplaceAllString(md, ""), -1) {
		if !strings.Contains(m[1], "\n\n") {
			out = append(out, strings.ReplaceAll(m[1], "\n", " "))
		}
	}
	return out
}

// migrateInvocationFlags returns the flag names passed to each
// stellarindex-migrate call in line, up to the end of that command.
func migrateInvocationFlags(line string) []string {
	toks := strings.Fields(line)
	var out []string
	for i, tok := range toks {
		if path.Base(strings.Trim(tok, `'"(`)) != "stellarindex-migrate" {
			continue
		}
		for _, a := range toks[i+1:] {
			if a == "|" || a == ";" || a == "&&" || strings.HasPrefix(a, ">") {
				break
			}
			if strings.HasPrefix(a, "-") {
				name, _, _ := strings.Cut(strings.TrimLeft(a, "-"), "=")
				out = append(out, name)
			}
		}
	}
	return out
}
