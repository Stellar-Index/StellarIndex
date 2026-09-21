package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestUsageRollupBackfillUsageDocumentsWriteFlag pins RLT-405 (audit
// 2026-09-18): the usage-rollup-backfill package doc's worked example
// must include -write. opsutil.RegisterWriteGate defaults to dry-run,
// so an operator who copy-pastes the documented invocation verbatim
// gets a silent no-op scan rather than the recovery they asked for.
func TestUsageRollupBackfillUsageDocumentsWriteFlag(t *testing.T) {
	src, err := os.ReadFile("usage_rollup_backfill.go")
	if err != nil {
		t.Fatalf("read usage_rollup_backfill.go: %v", err)
	}
	doc := extractUsageBlock(t, string(src))
	if !strings.Contains(doc, "-write") {
		t.Errorf("usage_rollup_backfill.go's documented invocation is missing -write; "+
			"as written it silently dry-runs. Usage block:\n%s", doc)
	}
}

// TestUsageRollupBackfillRunbookDocumentsCatchup pins T166 (audit
// 2026-09-18): the runbook this alert points operators at must name
// the usage-rollup-backfill catch-up tool and must NOT still assert
// that no catch-up step exists — that assertion is directly
// contradicted by cmd/stellarindex-ops/usage_rollup_backfill.go's own
// doc comment (COR-10): a day the live sweep's two-day window skips is
// permanently lost unless this tool is run.
func TestUsageRollupBackfillRunbookDocumentsCatchup(t *testing.T) {
	root := repoRootForOpsTest(t)
	runbook, err := os.ReadFile(filepath.Join(root, "docs/operations/runbooks/usage-rollup-failing.md"))
	if err != nil {
		t.Fatalf("read runbook: %v", err)
	}
	rb := string(runbook)
	if !strings.Contains(rb, "usage-rollup-backfill") {
		t.Error("docs/operations/runbooks/usage-rollup-failing.md never mentions usage-rollup-backfill, " +
			"the only recovery path for a day the live sweep's two-day window skips (COR-10)")
	}
	if strings.Contains(rb, `No operator "catch-up" step exists or is needed`) {
		t.Error(`runbook still claims 'No operator "catch-up" step exists or is needed', ` +
			"contradicted by cmd/stellarindex-ops/usage_rollup_backfill.go")
	}
}

// repoRootForOpsTest resolves the repo root from cmd/stellarindex-ops.
func repoRootForOpsTest(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root %s has no go.mod: %v", root, err)
	}
	return root
}

// extractUsageBlock returns the text between "Usage:" and the next
// blank doc-comment line in the package doc comment above
// usageRollupBackfill, i.e. exactly the invocation an operator would
// copy-paste.
func extractUsageBlock(t *testing.T, src string) string {
	t.Helper()
	i := strings.Index(src, "Usage:")
	if i < 0 {
		t.Fatal("usage_rollup_backfill.go has no 'Usage:' doc comment to check")
	}
	rest := src[i:]
	end := strings.Index(rest, "func usageRollupBackfill")
	if end < 0 {
		t.Fatal("could not bound the Usage: block before func usageRollupBackfill")
	}
	return rest[:end]
}
