package main

import (
	"os"
	"path/filepath"
	"testing"
)

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
