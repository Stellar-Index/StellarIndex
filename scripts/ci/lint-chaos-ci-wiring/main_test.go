package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLint_RealWorkflowsWireChaos — the real .github/workflows tree must
// reference the chaos suite. This is the green side of T430: before
// chaos-nightly.yml existed, this test failed against the real repo tree
// (see the package comment / commit history for the red run).
func TestLint_RealWorkflowsWireChaos(t *testing.T) {
	failures, summary, err := run("../../../.github/workflows/*.yml")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(failures) > 0 {
		t.Fatalf("expected the real workflow tree to wire in the chaos suite, got failures: %v", failures)
	}
	if summary == "" {
		t.Fatal("expected a non-empty summary on a clean run")
	}
}

// TestLint_FailsWhenNoWorkflowReferencesChaos reproduces the pre-T430
// state: a workflow directory with files present but none of them
// mentioning the chaos suite. This is exactly the shape of
// .github/workflows/*.yml before chaos-nightly.yml was added — grepping
// for 'chaos' returned zero matches.
func TestLint_FailsWhenNoWorkflowReferencesChaos(t *testing.T) {
	dir := t.TempDir()
	unrelated := "name: unrelated\non: push\njobs:\n  build:\n    runs-on: ubuntu-latest\n    steps:\n      - run: go build ./...\n"
	if err := os.WriteFile(filepath.Join(dir, "ci.yml"), []byte(unrelated), 0o600); err != nil {
		t.Fatal(err)
	}

	failures, _, err := run(filepath.Join(dir, "*.yml"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(failures) != 1 {
		t.Fatalf("expected exactly 1 failure for a workflow tree with no chaos wiring, got %d: %v", len(failures), failures)
	}
}

// TestLint_PassesWhenAWorkflowReferencesTheRunner is the direct green
// case: a workflow whose only relevant line is the run.sh invocation.
func TestLint_PassesWhenAWorkflowReferencesTheRunner(t *testing.T) {
	dir := t.TempDir()
	wired := "name: chaos-nightly\non:\n  schedule:\n    - cron: '17 3 * * *'\njobs:\n  chaos-run:\n    runs-on: ubuntu-latest\n    steps:\n      - run: ./test/chaos/run.sh\n"
	if err := os.WriteFile(filepath.Join(dir, "chaos-nightly.yml"), []byte(wired), 0o600); err != nil {
		t.Fatal(err)
	}

	failures, summary, err := run(filepath.Join(dir, "*.yml"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(failures) != 0 {
		t.Fatalf("expected no failures, got %v", failures)
	}
	if summary == "" {
		t.Fatal("expected a non-empty summary")
	}
}
