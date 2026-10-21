package guardscan

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// FixtureModule returns a temp module with this repo's module path and internal/worker linked to the real package, so
// the scanner's go.mod lookup and import mapping run for real without writing into the source tree, which other
// packages' tests walk in parallel. Exported for the external test package.
func FixtureModule(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module github.com/Stellar-Index/StellarIndex\n"), 0o600); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	worker, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "internal"), 0o750); err != nil {
		t.Fatalf("mkdir internal: %v", err)
	}
	if err := os.Symlink(worker, filepath.Join(root, "internal", "worker")); err != nil {
		t.Fatalf("link internal/worker: %v", err)
	}
	return root
}

// A Scanner reused across files must parse each package it resolves into once; re-indexing per file made the repo-wide
// guard walks quadratic and pushed them past the CI test timeout under -race.
func TestScanner_IndexesEachPackageOnce(t *testing.T) {
	dir := filepath.Join(FixtureModule(t), "fx", "shared")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
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
	// fx/shared once, internal/worker once — not once per file.
	if sc.parsedDirs != 2 {
		t.Errorf("indexed %d package directories scanning 3 files, want 2 (each package once)", sc.parsedDirs)
	}
}

// TestScan_RecursedFuncLitUsesCalleeFileSet: a site found by recursing into a callee must
// resolve Line and a FuncLit's Origin against the callee's own FileSet (indexDir/indexImport
// each build one), not the scanned file's r.fset, or it reports Line 0 and a garbage Origin.
func TestScan_RecursedFuncLitUsesCalleeFileSet(t *testing.T) {
	dir := filepath.Join(FixtureModule(t), "fx", "recursed_funclit")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "workers.go"), []byte(`package main

func startWorkers() {
	go func() {
		_ = 1
	}()
}
`), 0o600); err != nil {
		t.Fatalf("write workers.go: %v", err)
	}
	path := filepath.Join(dir, "main.go")
	if err := os.WriteFile(path, []byte(`package main

func run() {
	go startWorkers()
}
`), 0o600); err != nil {
		t.Fatalf("write main.go: %v", err)
	}

	sites, err := ScanFile(path, Config{})
	if err != nil {
		t.Fatalf("ScanFile: %v", err)
	}
	if len(sites) != 2 {
		t.Fatalf("found %d go statements, want 2 (go startWorkers() plus its inner literal); sites: %+v", len(sites), sites)
	}
	var lit Site
	found := false
	for _, s := range sites {
		if s.Kind == KindFuncLit {
			lit, found = s, true
		}
	}
	if !found {
		t.Fatalf("the recursed func literal site was not found; sites: %+v", sites)
	}
	const wantLine = 4 // workers.go's `go func() {` line
	if lit.Line != wantLine {
		t.Errorf("recursed func literal Line = %d, want %d (workers.go's real line, not 0 from the wrong FileSet)", lit.Line, wantLine)
	}
	wantOrigin := fmt.Sprintf("workers.go:%d", wantLine)
	if lit.Origin != wantOrigin {
		t.Errorf("recursed func literal Origin = %q, want %q", lit.Origin, wantOrigin)
	}
}
