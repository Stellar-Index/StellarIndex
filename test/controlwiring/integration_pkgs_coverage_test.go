package controlwiring

import (
	"bufio"
	"go/build/constraint"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ─── T424 / T449: every integration-tagged test must be in INT_TEST_PKGS ──
//
// A `//go:build integration` test is invisible to the unit job (`go test
// ./...` carries no tag), so the ONLY things that compile or run it are the
// three paths fed by the Makefile's INT_TEST_PKGS: `make test-integration`,
// `make test-integration-build` (the CI compile gate), and the CI shards
// (scripts/ci/integration-shard.sh derives its package list from `make
// print-int-test-pkgs`). A tagged test in a package that list does not cover
// is therefore compiled by nothing and executed by nothing — it can rot, or
// its subject can regress, with every gate green.
//
// That is how scripts/ops/fx-history-backfill/generation_test.go — the
// proven-red regression for an INV-3 money invariant (operator fx_quotes
// corrections must be stamped with a positive derive generation, or the next
// gen-0 worker refresh silently reverts them) — sat wired into zero build,
// test, or CI paths. F-1334 (cmd/stellarindex-ops) and W6-tst-1
// (internal/ops/archive) were the same defect found one package at a time;
// this test closes the class: it walks the tree for integration-gated test
// files and fails on any whose package INT_TEST_PKGS does not match.
//
// Untagged on purpose — it must run in the default suite, the one place a
// newly stranded test is guaranteed to be noticed.

var intTestPkgsLine = regexp.MustCompile(`(?m)^INT_TEST_PKGS\s*:?=\s*(.*)$`)

// intTestPkgPatterns returns the Makefile's INT_TEST_PKGS entries as
// slash-separated repo-relative patterns with the leading "./" removed
// (e.g. "test/integration/..."). It fails closed: a missing, duplicated or
// empty assignment would otherwise read as "nothing is covered" or, worse,
// let a second definition silently win.
func intTestPkgPatterns(t *testing.T, root string) []string {
	t.Helper()
	mk, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	matches := intTestPkgsLine.FindAllStringSubmatch(string(mk), -1)
	if len(matches) != 1 {
		t.Fatalf("want exactly one INT_TEST_PKGS assignment in Makefile, found %d", len(matches))
	}
	fields := strings.Fields(matches[0][1])
	if len(fields) == 0 {
		t.Fatal("INT_TEST_PKGS is empty")
	}
	patterns := make([]string, 0, len(fields))
	for _, f := range fields {
		if !strings.HasPrefix(f, "./") {
			t.Fatalf("INT_TEST_PKGS entry %q is not a ./-relative package pattern; this guard cannot reason about it", f)
		}
		patterns = append(patterns, strings.TrimPrefix(f, "./"))
	}
	return patterns
}

// pkgPatternCovers reports whether a go package pattern (leading "./"
// removed) matches the slash-separated repo-relative directory dir, with the
// go tool's semantics: "x/..." matches x and everything beneath it, a bare
// "x" matches x alone.
func pkgPatternCovers(pattern, dir string) bool {
	if base, ok := strings.CutSuffix(pattern, "/..."); ok {
		return dir == base || strings.HasPrefix(dir, base+"/")
	}
	return dir == pattern
}

// integrationGated reports whether the file at path is compiled under
// `-tags=integration` and NOT under the default tag set — i.e. whether the
// integration suite is the only thing that can ever build it. Host
// GOOS/GOARCH terms are treated as unsatisfied, which only errs toward
// leaving a platform-specific file out of scope rather than mis-flagging it.
func integrationGated(t *testing.T, path string) bool {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if constraint.IsGoBuild(line) {
			expr, err := constraint.Parse(line)
			if err != nil {
				t.Fatalf("parse build constraint in %s: %v", path, err)
			}
			withTag := expr.Eval(func(tag string) bool { return tag == "integration" })
			withoutTag := expr.Eval(func(string) bool { return false })
			return withTag && !withoutTag
		}
		// Build constraints must precede the package clause; stop there.
		if strings.HasPrefix(line, "package ") {
			return false
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan %s: %v", path, err)
	}
	return false
}

// integrationGatedTestDirs walks the repo and returns, per slash-separated
// repo-relative directory, one integration-gated _test.go file found in it.
func integrationGatedTestDirs(t *testing.T, root string) map[string]string {
	t.Helper()
	// Not Go source of this module: VCS metadata, nested agent checkouts
	// (full copies of the repo under .claude/), JS dependency trees, and
	// testdata (ignored by the go tool).
	skip := map[string]bool{".git": true, ".claude": true, "node_modules": true, "testdata": true, "vendor": true}

	dirs := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && skip[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), "_test.go") || !integrationGated(t, path) {
			return nil
		}
		rel, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		dir := filepath.ToSlash(rel)
		if _, seen := dirs[dir]; !seen {
			dirs[dir] = d.Name()
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return dirs
}

func TestEveryIntegrationTaggedTestIsInIntTestPkgs(t *testing.T) {
	root := repoRoot(t)
	patterns := intTestPkgPatterns(t, root)
	dirs := integrationGatedTestDirs(t, root)

	// Non-vacuity: a walk or a constraint parser that finds nothing would
	// pass this test over an empty set. These two are known members — the
	// main suite, and the INV-3 test this guard was written for.
	for _, want := range []string{"test/integration", "scripts/ops/fx-history-backfill"} {
		if _, ok := dirs[want]; !ok {
			t.Fatalf("walk found no integration-gated test in %s — the detector is broken "+
				"(or the test moved: update this list, do not delete the check)", want)
		}
	}

	for dir, file := range dirs {
		covered := false
		for _, p := range patterns {
			if pkgPatternCovers(p, dir) {
				covered = true
				break
			}
		}
		if !covered {
			t.Errorf("%s/%s is `//go:build integration` but ./%s is matched by no INT_TEST_PKGS entry (%s): "+
				"no build, test or CI path compiles or runs it. Add the package to INT_TEST_PKGS in the Makefile.",
				dir, file, dir, strings.Join(patterns, " "))
		}
	}
}

func TestPkgPatternCovers(t *testing.T) {
	cases := []struct {
		pattern, dir string
		want         bool
	}{
		{"test/integration/...", "test/integration", true},
		{"test/integration/...", "test/integration/sub", true},
		{"test/integration/...", "test/integrationx", false},
		{"scripts/ops/...", "scripts/ops/fx-history-backfill", true},
		{"scripts/ops/...", "scripts/ci/lint-openapi-urls", false},
		{"cmd/stellarindex-ops", "cmd/stellarindex-ops", true},
		{"cmd/stellarindex-ops", "cmd/stellarindex-ops/sub", false},
	}
	for _, c := range cases {
		if got := pkgPatternCovers(c.pattern, c.dir); got != c.want {
			t.Errorf("pkgPatternCovers(%q, %q) = %v, want %v", c.pattern, c.dir, got, c.want)
		}
	}
}
