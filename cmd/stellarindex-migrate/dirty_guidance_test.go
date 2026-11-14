package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDirtyGuidanceNamesTheForceTarget: the guidance must name the version just below the failed one, which is the
// only force argument that lets `up` run that migration again.
func TestDirtyGuidanceNamesTheForceTarget(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"0001_a.up.sql", "0002_b.up.sql", "0005_c.up.sql", "0005_c.down.sql"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("SELECT 1;"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for v, want := range map[int]string{5: "2", 2: "1", 1: "none"} {
		if got := previousMigration(dir, v); got != want {
			t.Errorf("previousMigration(%d) = %q, want %q", v, got, want)
		}
	}
	g := dirtyGuidance(dir, 5)
	if !strings.Contains(g, "migration 5 failed") || !strings.Contains(g, "force 2`") {
		t.Errorf("guidance = %q, want it to name migration 5 and `force 2`", g)
	}
}
