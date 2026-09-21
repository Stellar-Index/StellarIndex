package ingest

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TestBackfillProcedureDocReferencesExistingGoFiles pins RLT-203
// (reverification 2026-09-18): docs/operations/backfill-procedure.md
// used to claim "The CLI lives at `cmd/stellarindex-ops/backfill.go`" —
// that file was removed by the maintainability split that created this
// package (internal/ops/ingest), and never existed under that path
// again. An operator following the runbook link got a 404. Every
// `.go` path the runbook cites, backtick-quoted or as a markdown link
// target, must exist on disk.
func TestBackfillProcedureDocReferencesExistingGoFiles(t *testing.T) {
	root := ingestDocsRepoRoot(t)
	doc, err := os.ReadFile(filepath.Join(root, "docs/operations/backfill-procedure.md"))
	if err != nil {
		t.Fatalf("read backfill-procedure.md: %v", err)
	}

	backtick := regexp.MustCompile("`([A-Za-z0-9_./-]+\\.go)`")
	link := regexp.MustCompile(`\]\(((?:\.\./)+[A-Za-z0-9_./-]+\.go)\)`)

	seen := map[string]bool{}
	for _, m := range backtick.FindAllStringSubmatch(string(doc), -1) {
		seen[m[1]] = true
	}
	for _, m := range link.FindAllStringSubmatch(string(doc), -1) {
		// Link targets are relative to the doc's own directory
		// (docs/operations/), not the repo root.
		resolved := filepath.Clean(filepath.Join("docs", "operations", m[1]))
		seen[resolved] = true
	}

	if len(seen) == 0 {
		t.Fatal("no .go references found in backfill-procedure.md — regex drifted, fix the test")
	}
	for path := range seen {
		if _, err := os.Stat(filepath.Join(root, path)); err != nil {
			t.Errorf("backfill-procedure.md references %q, which does not exist at the repo root: %v", path, err)
		}
	}
}

// ingestDocsRepoRoot resolves the repo root from internal/ops/ingest.
func ingestDocsRepoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root %s has no go.mod: %v", root, err)
	}
	return root
}
