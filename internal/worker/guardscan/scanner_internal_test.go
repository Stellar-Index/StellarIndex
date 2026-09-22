package guardscan

import (
	"os"
	"path/filepath"
	"testing"
)

// A Scanner reused across files must parse each package it resolves into
// once. Re-indexing the scanned file's own package per file made the
// repo-wide guard walks quadratic and pushed their packages past the CI
// test timeout under -race.
func TestScanner_IndexesEachPackageOnce(t *testing.T) {
	dir := filepath.Join("testdata", "shared")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(dir)
		_ = os.Remove("testdata")
	})
	files := map[string]string{
		"a.go": `package shared

import "log/slog"

func startA(logger *slog.Logger) { go guarded(logger) }
`,
		"b.go": `package shared

import (
	"log/slog"

	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

func startB(logger *slog.Logger) { go bare(logger) }

func guarded(logger *slog.Logger) {
	defer worker.Recover(logger, "guarded")
}

func bare(*slog.Logger) {}
`,
		"c.go": `package shared

import (
	"log/slog"

	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

func startC(logger *slog.Logger) { go worker.Recover(logger, "x") }
`,
	}
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	sc := NewScanner(Config{Guards: []string{"worker.Recover"}})
	got := map[string]Site{}
	for _, name := range []string{"a.go", "b.go", "c.go"} {
		sites, err := sc.ScanFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("ScanFile(%s): %v", name, err)
		}
		if len(sites) != 1 {
			t.Fatalf("%s: got %d sites, want 1", name, len(sites))
		}
		got[name] = sites[0]
	}

	// Sharing the index must not change what each file resolves to.
	if s := got["a.go"]; s.Kind != KindPackageFunc || !s.Recovers || s.Origin != "b.go:11" {
		t.Errorf("a.go: go guarded() = %+v, want package-func resolved to b.go:11 and recovering", s)
	}
	if s := got["b.go"]; s.Kind != KindPackageFunc || s.Recovers {
		t.Errorf("b.go: go bare() = %+v, want package-func NOT recovering", s)
	}
	if s := got["c.go"]; s.Kind != KindImportedFunc {
		t.Errorf("c.go: go worker.Recover() = %+v, want imported-func", s)
	}
	// testdata/shared once, internal/worker once — not once per file.
	if sc.parsedDirs != 2 {
		t.Errorf("indexed %d package directories scanning 3 files, want 2 (each package once)", sc.parsedDirs)
	}
}
