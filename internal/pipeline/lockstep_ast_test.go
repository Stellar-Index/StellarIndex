// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package pipeline

// This file is the REAL lockstep guard the IsProjectedEvent comment
// used to attribute to a (never-built) "ADR-0030 lint". Five sites
// must agree for a source's data to flow (ADR-0031/0032):
//
//   sink.go HandleEvent      — persist arm per consumer.Event type
//   sink.go IsProjectedEvent — projected-event membership
//   sink.go tradeFromEvent   — trade-shaped fast path
//   projector/registry.go    — buildSource case per projected source
//   pipeline/dispatcher.go   — BuildDispatcher decoder registration
//
// Drift between them is SILENT DATA LOSS (F-1316: the projector
// wrote zero sep41_transfers rows because one list was missed).
// These tests parse the actual switch statements + source packages
// with go/ast and cross-check, so adding an event type or source
// without completing the wiring fails CI instead of dropping rows.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// notProjectedEvents is the conscious-decision register for
// consumer.Event types defined in PROJECTED source packages that are
// deliberately NOT projected. Keep reasons — every entry is a design
// decision, not a default.
var notProjectedEvents = map[string]string{
	// (none today — every event type a projected source package
	// defines is projector-owned. If you add a log-only or
	// dispatcher-owned event type to a projected source package,
	// register it here with the ADR/why.)
}

// notSunkEvents is the conscious-decision register for consumer.Event
// types under internal/sources that deliberately have NO persist arm
// in sink.go's HandleEvent (i.e. some OTHER writer owns them and they
// never reach HandleEvent). Empty today — every source event type is
// sunk by HandleEvent. If you ever add an event type handled entirely
// off the HandleEvent path, register it here with the reason so the
// exhaustiveness guard below stays a real signal rather than being
// loosened wholesale.
var notSunkEvents = map[string]string{
	"classicmovements.MovementEvent": "ADR-0047 D2: historical-only, lake-derived, pre-P23 classic-movement reconstruction. Its Decoder is never registered with the live dispatcher (nothing to decode live — the P23 boundary is a hard upper bound), so MovementEvent never flows through HandleEvent. The sole writer is `stellarindex-ops classic-movements-backfill` (internal/ops/chops), which streams clickhouse.ClassicOp -> classicmovements.Decoder -> timescale.Store.BatchInsertClassicMovements directly, bypassing pipeline.HandleEvent entirely.",
}

// parseFile parses one Go file into an AST.
func parseFile(t *testing.T, fset *token.FileSet, path string) *ast.File {
	t.Helper()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return f
}

// funcDecl finds a top-level function by name.
func funcDecl(t *testing.T, f *ast.File, name string) *ast.FuncDecl {
	t.Helper()
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == name && fd.Recv == nil {
			return fd
		}
	}
	t.Fatalf("function %s not found", name)
	return nil
}

// caseTypeNames extracts the `pkg.Type` names listed across all case
// clauses of the FIRST type-switch inside fn.
func caseTypeNames(t *testing.T, fn *ast.FuncDecl) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	ast.Inspect(fn, func(n ast.Node) bool {
		sw, ok := n.(*ast.TypeSwitchStmt)
		if !ok {
			return true
		}
		for _, stmt := range sw.Body.List {
			cc, ok := stmt.(*ast.CaseClause)
			if !ok {
				continue
			}
			for _, expr := range cc.List {
				if sel, ok := expr.(*ast.SelectorExpr); ok {
					if pkg, ok := sel.X.(*ast.Ident); ok {
						out[pkg.Name+"."+sel.Sel.Name] = true
					}
				}
			}
		}
		return false // first type-switch only
	})
	if len(out) == 0 {
		t.Fatalf("no type-switch cases found in %s", fn.Name.Name)
	}
	return out
}

// importPathByIdent maps local package idents (soroswap, blend_backstop,
// …) to their import paths, from a file's import block.
func importPathByIdent(f *ast.File) map[string]string {
	out := map[string]string{}
	for _, imp := range f.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		ident := path[strings.LastIndex(path, "/")+1:]
		if imp.Name != nil {
			ident = imp.Name.Name
		}
		out[ident] = path
	}
	return out
}

// eventTypesInPackage enumerates the exported types in a package dir
// that implement consumer.Event (detected by an `EventKind() string`
// method — the interface's distinctive member).
func eventTypesInPackage(t *testing.T, dir string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f := parseFile(t, fset, filepath.Join(dir, e.Name()))
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Name.Name != "EventKind" || fd.Recv == nil || len(fd.Recv.List) == 0 {
				continue
			}
			recv := fd.Recv.List[0].Type
			if star, ok := recv.(*ast.StarExpr); ok {
				recv = star.X
			}
			if id, ok := recv.(*ast.Ident); ok && ast.IsExported(id.Name) {
				out[id.Name] = true
			}
		}
	}
	return out
}

// repoDir resolves a repo-relative path from this package dir.
func repoDir(parts ...string) string {
	return filepath.Join(append([]string{"..", ".."}, parts...)...)
}

// registryCasePackages extracts the package idents used in
// buildSource's `case <pkg>.SourceName:` clauses.
func registryCasePackages(t *testing.T) (pkgs map[string]bool, imports map[string]string) {
	t.Helper()
	fset := token.NewFileSet()
	f := parseFile(t, fset, repoDir("internal", "projector", "registry.go"))
	fn := funcDecl(t, f, "buildSource")
	pkgs = map[string]bool{}
	ast.Inspect(fn, func(n ast.Node) bool {
		sw, ok := n.(*ast.SwitchStmt)
		if !ok {
			return true
		}
		for _, stmt := range sw.Body.List {
			cc, ok := stmt.(*ast.CaseClause)
			if !ok {
				continue
			}
			for _, expr := range cc.List {
				if sel, ok := expr.(*ast.SelectorExpr); ok {
					if pkg, ok := sel.X.(*ast.Ident); ok {
						pkgs[pkg.Name] = true
					}
				}
			}
		}
		return false
	})
	if len(pkgs) == 0 {
		t.Fatal("no case packages found in buildSource")
	}
	return pkgs, importPathByIdent(f)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestLockstep_ProjectedEventsHavePersistArms — every type
// IsProjectedEvent claims must have a HandleEvent persist arm, and
// every tradeFromEvent fast-path type must too. A projected event
// without a persist arm reaches the projector and is dropped by
// HandleEvent's default (silent loss).
func TestLockstep_ProjectedEventsHavePersistArms(t *testing.T) {
	fset := token.NewFileSet()
	sink := parseFile(t, fset, "sink.go")

	handle := caseTypeNames(t, funcDecl(t, sink, "HandleEvent"))
	projected := caseTypeNames(t, funcDecl(t, sink, "IsProjectedEvent"))
	trades := caseTypeNames(t, funcDecl(t, sink, "tradeFromEvent"))

	for _, typ := range sortedKeys(projected) {
		if !handle[typ] {
			t.Errorf("IsProjectedEvent lists %s but HandleEvent has no persist arm — projected rows for it are silently dropped", typ)
		}
	}
	for _, typ := range sortedKeys(trades) {
		if !handle[typ] {
			t.Errorf("tradeFromEvent lists %s but HandleEvent has no arm — batch path and slow path disagree", typ)
		}
	}
}

// TestLockstep_RegistrySourcesFullyWired — the F-1316 guard. For
// every source package registered in projector buildSource:
//   - every consumer.Event type that package defines must appear in
//     IsProjectedEvent (or carry a notProjectedEvents entry), else
//     Phase-4 ingest drops its rows silently;
//   - and IsProjectedEvent must not reference types that no longer
//     exist in the package (stale entry after a rename).
func TestLockstep_RegistrySourcesFullyWired(t *testing.T) {
	fset := token.NewFileSet()
	sink := parseFile(t, fset, "sink.go")
	projected := caseTypeNames(t, funcDecl(t, sink, "IsProjectedEvent"))

	regPkgs, regImports := registryCasePackages(t)

	// Package ident → set of event type names IsProjectedEvent knows.
	projByPkg := map[string]map[string]bool{}
	for full := range projected {
		parts := strings.SplitN(full, ".", 2)
		if projByPkg[parts[0]] == nil {
			projByPkg[parts[0]] = map[string]bool{}
		}
		projByPkg[parts[0]][parts[1]] = true
	}

	const modPrefix = "github.com/StellarIndex/stellar-index/"

	for _, pkg := range sortedKeys(regPkgs) {
		impPath, ok := regImports[pkg]
		if !ok {
			t.Errorf("registry package %q has no import mapping", pkg)
			continue
		}
		dir := repoDir(filepath.FromSlash(strings.TrimPrefix(impPath, modPrefix)))
		evTypes := eventTypesInPackage(t, dir)
		if len(evTypes) == 0 {
			t.Errorf("projected source package %s defines no consumer.Event types — registry entry stale?", pkg)
			continue
		}
		for _, typ := range sortedKeys(evTypes) {
			full := pkg + "." + typ
			if projByPkg[pkg][typ] {
				continue
			}
			if _, allowed := notProjectedEvents[full]; allowed {
				continue
			}
			t.Errorf("%s implements consumer.Event in a PROJECTED source package but is missing from IsProjectedEvent — Phase-4 ingest will silently drop it (F-1316 class). Add it there (and a HandleEvent arm), or register it in notProjectedEvents with a reason", full)
		}
		// Reverse: stale IsProjectedEvent entries.
		for typ := range projByPkg[pkg] {
			if !evTypes[typ] {
				t.Errorf("IsProjectedEvent lists %s.%s but the package defines no such consumer.Event type — stale entry", pkg, typ)
			}
		}
	}

	// Every package IsProjectedEvent references must be a registered
	// projector source (else its events are skipped by the sink but
	// NO projector writes them — total loss).
	for pkg := range projByPkg {
		if !regPkgs[pkg] {
			t.Errorf("IsProjectedEvent references package %q which has no buildSource case in projector/registry.go — its events are skipped by the sink AND never projected", pkg)
		}
	}

	// Stale allowlist entries.
	for full := range notProjectedEvents {
		pkg := strings.SplitN(full, ".", 2)[0]
		if !regPkgs[pkg] {
			t.Errorf("notProjectedEvents entry %q references a package that is not a projected source — stale entry", full)
		}
	}
}

// allSourceEventTypes walks internal/sources recursively and returns
// the set of "pkg.Type" names for every consumer.Event implementer
// (a type with an EventKind() method), keyed by DIRECTORY BASENAME —
// which is exactly the import ident sink.go's HandleEvent uses for
// every source package (underscore dirs are import-aliased to their
// basename throughout the repo). This is the authoritative universe
// of event types the sink's type-switch must cover.
func allSourceEventTypes(t *testing.T) map[string]bool {
	t.Helper()
	root := repoDir("internal", "sources")
	out := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, de fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !de.IsDir() {
			return nil
		}
		base := filepath.Base(path)
		for typ := range eventTypesInPackage(t, path) {
			out[base+"."+typ] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if len(out) == 0 {
		t.Fatalf("found no consumer.Event implementers under %s — walker broken?", root)
	}
	return out
}

// TestLockstep_EveryConsumerEventHasSinkArm is the exhaustiveness guard
// for the "pipeline sink type-switch trap" (BACKLOG #56): EVERY type
// that implements consumer.Event under internal/sources MUST have a
// persist arm in sink.go's HandleEvent. A missing arm means the event
// falls through to HandleEvent's `default` and is counted as an
// "unhandled event kind" drop instead of being persisted — the exact
// silent-loss failure mode where "metrics say we're ingesting but the
// tables stay empty".
//
// The sibling guards (TestLockstep_ProjectedEventsHavePersistArms,
// TestLockstep_RegistrySourcesFullyWired) only cover PROJECTED source
// packages, reached via IsProjectedEvent. This one covers ALL source
// packages — the non-projected ones too (sdex / external / band /
// soroswap_router / the five supply observers), which previously had
// no automated guard tying them to a HandleEvent arm. Adding a new
// consumer.Event type without wiring the sink now fails CI here rather
// than silently dropping its rows in production.
func TestLockstep_EveryConsumerEventHasSinkArm(t *testing.T) {
	fset := token.NewFileSet()
	sink := parseFile(t, fset, "sink.go")
	handle := caseTypeNames(t, funcDecl(t, sink, "HandleEvent"))

	all := allSourceEventTypes(t)
	for _, full := range sortedKeys(all) {
		if handle[full] {
			continue
		}
		if _, allowed := notSunkEvents[full]; allowed {
			continue
		}
		t.Errorf("%s implements consumer.Event but sink.go HandleEvent has no persist arm — it falls to the 'unhandled event kind' default and is dropped (pipeline sink type-switch trap, #56). Add a case in HandleEvent (and the tradeFromEvent fast-path if it is trade-shaped), or register it in notSunkEvents with a reason", full)
	}
}
