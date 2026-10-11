// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package pipeline

// Three sites must agree for a source's data to flow (ADR-0031/0032): source_spec.go (one SourceSpec per source),
// sink.go HandleEvent (a persist arm per consumer.Event type), and sink.go tradeFromEvent (trade fast path).
// Drift is silent data loss; these tests cross-check them with go/ast so incomplete wiring fails CI.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
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

// notSunkEvents registers consumer.Event types that deliberately have NO HandleEvent arm (another writer owns them), with the reason.
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

// caseTypeNames extracts the `pkg.Type` names of every case clause of every type-switch in fn, descending into nested switches.
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
		return true
	})
	if len(out) == 0 {
		t.Fatalf("no type-switch cases found in %s", fn.Name.Name)
	}
	return out
}

// caseBodyPersists reports per case label whether the body calls a persist* helper or store.<Method>, since a bare `case X: return nil` would silently drop the event.
func caseBodyPersists(t *testing.T, fn *ast.FuncDecl) map[string]bool {
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
			persists := bodyCallsPersist(cc.Body)
			for _, expr := range cc.List {
				if sel, ok := expr.(*ast.SelectorExpr); ok {
					if pkg, ok := sel.X.(*ast.Ident); ok {
						name := pkg.Name + "." + sel.Sel.Name
						out[name] = out[name] || persists
					}
				}
			}
		}
		return true
	})
	return out
}

// caseTypesCalling returns the `pkg.Type` case labels of fn's type
// switches whose body calls the plain function callee(...).
func caseTypesCalling(t *testing.T, fn *ast.FuncDecl, callee string) map[string]bool {
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
			calls := false
			for _, s := range cc.Body {
				ast.Inspect(s, func(n ast.Node) bool {
					if call, ok := n.(*ast.CallExpr); ok {
						if id, ok := call.Fun.(*ast.Ident); ok && id.Name == callee {
							calls = true
						}
					}
					return true
				})
			}
			if !calls {
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
		return true
	})
	return out
}

// bodyCallsPersist reports whether stmts (a case clause's body)
// contains, anywhere in its subtree, a call to a `persist*` helper or
// a `store.<Method>(...)` call — the two shapes every real HandleEvent
// arm uses (see sink.go's handleEvent).
func bodyCallsPersist(stmts []ast.Stmt) bool {
	found := false
	for _, stmt := range stmts {
		ast.Inspect(stmt, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch callee := call.Fun.(type) {
			case *ast.Ident:
				if strings.HasPrefix(callee.Name, "persist") {
					found = true
				}
			case *ast.SelectorExpr:
				if id, ok := callee.X.(*ast.Ident); ok && id.Name == "store" {
					found = true
				}
			}
			return true
		})
	}
	return found
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

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestLockstep_ProjectedEventsHavePersistArms: every projected SourceSpec type and tradeFromEvent type needs a HandleEvent persist arm.
func TestLockstep_ProjectedEventsHavePersistArms(t *testing.T) {
	fset := token.NewFileSet()
	sink := parseFile(t, fset, "sink.go")

	// `handleEvent` (unexported) holds the dispatch type-switch;
	// HandleEvent is the exported one-line wrapper over it (split
	// out so an infra RETRY can suppress the once-per-event source
	// counters). The guard walks the switch wherever it lives.
	handleFn := funcDecl(t, sink, "handleEvent")
	handle := caseTypeNames(t, handleFn)
	persists := caseBodyPersists(t, handleFn)
	projected := specEventNames(true)
	trades := caseTypeNames(t, funcDecl(t, sink, "tradeFromEvent"))

	for _, typ := range sortedKeys(projected) {
		if !handle[typ] {
			t.Errorf("a projected SourceSpec lists %s but HandleEvent has no persist arm — projected rows for it are silently dropped", typ)
			continue
		}
		if !persists[typ] {
			t.Errorf("a projected SourceSpec lists %s and HandleEvent has a case for it, but the case body never calls a persist helper or store method — the arm drops the event instead of writing it (F-1316 class)", typ)
		}
	}
	for _, typ := range sortedKeys(trades) {
		if !handle[typ] {
			t.Errorf("tradeFromEvent lists %s but HandleEvent has no arm — batch path and slow path disagree", typ)
		}
	}
	callers := caseTypesCalling(t, handleFn, "persistTrade")
	if len(callers) == 0 {
		t.Fatal("no HandleEvent arm calls persistTrade — renamed? update this guard")
	}
	for _, typ := range sortedKeys(callers) {
		if !trades[typ] {
			t.Errorf("HandleEvent arm for %s calls persistTrade but tradeFromEvent omits it — the event silently takes the slow per-event path", typ)
		}
	}
}

// specEventNames names the event types the specs list ("pkg.Type",
// keyed by package dir basename as sink.go imports them): the projected
// ones when projected is true, the dispatcher-written ones otherwise.
func specEventNames(projected bool) map[string]bool {
	out := map[string]bool{}
	for i := range specs {
		if (specs[i].Projector != nil) != projected {
			continue
		}
		for _, ev := range specs[i].Events {
			typ := reflect.TypeOf(ev)
			out[path.Base(typ.PkgPath())+"."+typ.Name()] = true
		}
	}
	return out
}

// unlistedEventError explains why an event type is unwired, or returns "".
// In a projected package only a projected spec or a notProjectedEvents entry clears it; a non-projected listing is a silent Phase-4 drop.
func unlistedEventError(full string, projectedPkg bool, projected, dispatched map[string]bool, allow map[string]string) string {
	if projected[full] {
		return ""
	}
	if !projectedPkg {
		if dispatched[full] {
			return ""
		}
		return fmt.Sprintf("%s implements consumer.Event but no SourceSpec lists it. Add it to the spec's Events", full)
	}
	if _, ok := allow[full]; ok {
		return ""
	}
	return fmt.Sprintf("%s implements consumer.Event in a PROJECTED source package but no projected SourceSpec lists it — Phase-4 ingest silently drops it (F-1316 class). Add it to the projected spec's Events, or register it in notProjectedEvents with a reason", full)
}

// TestLockstep_SpecsListEveryEventType: every consumer.Event type in a spec's package must be listed by a spec (projected package: a projected spec or a notProjectedEvents entry),
// and each spec lists at least one event with a decoder for every writer it claims.
func checkSpecShape(t *testing.T, s *SourceSpec) {
	t.Helper()
	if len(s.Events) == 0 {
		t.Errorf("spec %s lists no event types", s.Name)
	}
	if s.NewDecoder == nil && s.Dispatch == nil {
		t.Errorf("spec %s has no decoder for the dispatcher", s.Name)
	}
	if s.Projector != nil && s.NewDecoder == nil && s.Projector.NewDecoder == nil {
		t.Errorf("spec %s is projected but has no decoder for the projector", s.Name)
	}
}

func TestLockstep_SpecsListEveryEventType(t *testing.T) {
	projected, dispatched := specEventNames(true), specEventNames(false)
	pkgDirs := map[string]bool{}       // import path -> projected
	projectedPkgs := map[string]bool{} // package basename, for stale allowlist entries
	const modPrefix = "github.com/Stellar-Index/StellarIndex/"
	for i := range specs {
		s := &specs[i]
		checkSpecShape(t, s)
		for _, ev := range s.Events {
			imp := reflect.TypeOf(ev).PkgPath()
			pkgDirs[imp] = pkgDirs[imp] || s.Projector != nil
		}
	}
	for _, imp := range sortedKeys(pkgDirs) {
		pkg := path.Base(imp)
		dir := repoDir(filepath.FromSlash(strings.TrimPrefix(imp, modPrefix)))
		for _, typ := range sortedKeys(eventTypesInPackage(t, dir)) {
			if msg := unlistedEventError(pkg+"."+typ, pkgDirs[imp], projected, dispatched, notProjectedEvents); msg != "" {
				t.Error(msg)
			}
		}
		if pkgDirs[imp] {
			projectedPkgs[pkg] = true
		}
	}
	for full := range notProjectedEvents {
		if projected[full] {
			t.Errorf("notProjectedEvents entry %q is listed by a projected spec — stale entry", full)
		}
		if !projectedPkgs[strings.SplitN(full, ".", 2)[0]] {
			t.Errorf("notProjectedEvents entry %q names no projected source package — stale entry", full)
		}
	}
}

// TestUnlistedEventError_ProjectedPackageNeedsProjectedSpec pins the
// allowlist's strength: a projected package's type listed only by a
// non-projected spec still needs a notProjectedEvents entry.
func TestUnlistedEventError_ProjectedPackageNeedsProjectedSpec(t *testing.T) {
	projected := map[string]bool{"p.Projected": true}
	dispatched := map[string]bool{"p.Dispatched": true, "d.Dispatched": true}
	allow := map[string]string{"p.Allowed": "reason"}
	for _, tc := range []struct {
		full         string
		projectedPkg bool
		ok           bool
	}{
		{"p.Projected", true, true},
		{"p.Allowed", true, true},
		{"p.Dispatched", true, false},
		{"p.Unlisted", true, false},
		{"d.Dispatched", false, true},
		{"d.Unlisted", false, false},
	} {
		if got := unlistedEventError(tc.full, tc.projectedPkg, projected, dispatched, allow) == ""; got != tc.ok {
			t.Errorf("%s (projected package %v): cleared = %v, want %v", tc.full, tc.projectedPkg, got, tc.ok)
		}
	}
}

// allSourceEventTypes returns every consumer.Event implementer under internal/sources as "pkg.Type", keyed by directory basename (the import ident HandleEvent uses).
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

// TestLockstep_EveryConsumerEventHasSinkArm: EVERY consumer.Event type under internal/sources needs a HandleEvent arm, or it hits `default` and is dropped.
// Unlike its siblings it covers non-projected packages too (sdex, external, band, soroswap_router, supply observers).
func TestLockstep_EveryConsumerEventHasSinkArm(t *testing.T) {
	fset := token.NewFileSet()
	sink := parseFile(t, fset, "sink.go")
	// See TestLockstep_ProjectedEventsHavePersistArms: the dispatch
	// type-switch lives in the unexported handleEvent body.
	handleFn := funcDecl(t, sink, "handleEvent")
	handle := caseTypeNames(t, handleFn)
	persists := caseBodyPersists(t, handleFn)

	all := allSourceEventTypes(t)
	for _, full := range sortedKeys(all) {
		if handle[full] {
			if !persists[full] {
				t.Errorf("%s has a case in sink.go HandleEvent but its body never calls a persist helper or store method — the arm is present but drops the event instead of writing it (pipeline sink type-switch trap, #56, F-1316 class)", full)
			}
			continue
		}
		if _, allowed := notSunkEvents[full]; allowed {
			continue
		}
		t.Errorf("%s implements consumer.Event but sink.go HandleEvent has no persist arm — it falls to the 'unhandled event kind' default and is dropped (pipeline sink type-switch trap). Add a case in HandleEvent (and the tradeFromEvent fast-path if it is trade-shaped), or register it in notSunkEvents with a reason", full)
	}
}
