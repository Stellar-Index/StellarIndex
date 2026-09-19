package controlwiring

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// ─── T424: docs/contributing/local-verification.md must name every
// directory that trips the `integration` change class ───
//
// The doc's "diff touches / jobs that do real work" table is a
// human-readable restatement of scripts/ci/check-change-class.sh's
// class_integration() (itself the offline mirror of ci.yml preflight's
// `integration` path filter — see TestT424TriggerCoversEveryIntTestPkg).
// Nothing lints this table's prose against the shell script, so when
// check-change-class.sh grew four directories for T424/F-1334/W6-tst-1
// (internal/ops/archive, cmd/stellarindex-ops, scripts/ops, test/harness)
// the doc table was not updated with it: a contributor reading the doc
// alone would believe a scripts/ops-only or cmd/stellarindex-ops-only diff
// never triggers the Docker integration shard, which is now false.

// docIntegrationTableRow finds the single line of
// docs/contributing/local-verification.md's path-filter table naming the
// `integration-test-shard` job, and returns the backtick-quoted globs in
// its "diff touches" column.
func docIntegrationTableRow(t *testing.T, root string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "docs", "contributing", "local-verification.md"))
	if err != nil {
		t.Fatalf("read local-verification.md: %v", err)
	}
	lineRE := regexp.MustCompile(`(?m)^\|(.+)\|\s*` + "`" + `integration-test-shard` + "`" + `[^|]*\|\s*$`)
	m := lineRE.FindStringSubmatch(string(raw))
	if m == nil {
		t.Fatal("local-verification.md: could not find the `integration-test-shard` table row")
	}
	globRE := regexp.MustCompile("`([^`]+)`")
	matches := globRE.FindAllStringSubmatch(m[1], -1)
	if len(matches) == 0 {
		t.Fatal("local-verification.md: integration-test-shard row lists no backtick-quoted globs")
	}
	globs := make([]string, 0, len(matches))
	for _, mm := range matches {
		globs = append(globs, mm[1])
	}
	return globs
}

// TestLocalVerificationDocMatchesIntegrationChangeClass mirrors
// TestT424TriggerCoversEveryIntTestPkg's probe strategy against the DOC
// table instead of a CI classifier: every directory check-change-class.sh's
// class_integration() matches must also be named (as a prefix glob) in the
// doc's table, or the doc misleads a contributor about which of their
// changes will run the Docker suite.
func TestLocalVerificationDocMatchesIntegrationChangeClass(t *testing.T) {
	root := repoRoot(t)
	classRE := checkChangeClassIntegrationRE(t, root)
	docGlobs := docIntegrationTableRow(t, root)

	// The directories check-change-class.sh's class_integration() names
	// today (scripts/ci/check-change-class.sh's own "Classes" comment block
	// is the source of truth). One probe path per directory, mirroring
	// probePaths()'s "one file directly inside the directory" shape.
	dirs := []string{
		"internal/storage",
		"internal/pipeline",
		"internal/sources",
		"internal/api",
		"internal/ops/archive",
		"cmd/stellarindex-ops",
		"migrations",
		"scripts/ops",
		"test/integration",
		"test/harness",
	}
	for _, dir := range dirs {
		probe := dir + "/probe.go"
		if !classRE.MatchString(probe) {
			t.Fatalf("check-change-class.sh class_integration no longer matches %s — update this test's dirs list first", probe)
		}
		if !globsFire(docGlobs, probe) {
			t.Errorf("docs/contributing/local-verification.md's integration-test-shard table row does not list %s "+
				"(doc globs: %v) — a contributor reading only the doc would not expect a %s change to run the "+
				"Docker integration suite", dir, docGlobs, dir)
		}
	}
	// go.mod is named as an exact file, not a directory glob.
	if !globsFire(docGlobs, "go.mod") {
		t.Errorf("docs/contributing/local-verification.md's integration-test-shard table row does not list go.mod (doc globs: %v)", docGlobs)
	}
}
