package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Source-level tripwire (the decimals_boot_guard_test.go /
// freeze_wiring_guard_test.go pattern): the retrying lake dial is only
// worth anything if the wiring goes through it. The call sites live inside
// run(), which needs a config file, a Postgres pool and a Redis client
// before they are reachable, so the WIRING is asserted here.
//
// The property pinned is "no component of this binary dials the lake
// inline", not a call count of one constructor in one file: every non-test
// file of the package is scanned, and a lake dial is any clickhouse
// constructor that takes a context — derived from the clickhouse package's
// own source, so NewExplorerReaderAuth, a future New*Reader, or an aliased
// import cannot slip past it.
func TestLakeReadersAreDialledWithRetry(t *testing.T) {
	c := censusLakeDials(t, ".", lakeDialConstructors(t, clickhouseSourceDir))

	if c.viaDecimals != 1 {
		t.Errorf("found %d dialDecimalsResolver call(s) in package main, want exactly 1 — a "+
			"single dial at startup is what disabled the decimals-assumption guard for the "+
			"whole process lifetime whenever ClickHouse was still loading metadata after a "+
			"reboot, so the guard's resolver must go through the retrying dial", c.viaDecimals)
	}
	// dialDecimalsResolver's own delegation + the MEV tx-order resolver +
	// the priceless-coverage SAC resolver (K024) + the supply refresher's
	// close-time reader on a real (non-dry-run) boot, via
	// newLazyCloseTimeReader (GH-902): that dial used to be a single
	// synchronous `return err` on failure, aborting the whole aggregator
	// over a transient ClickHouse blip.
	const wantViaGenericRetry = 4
	if c.viaGeneric != wantViaGenericRetry {
		t.Errorf("found %d dialLakeReaderWithRetry call(s) in package main, want %d — the MEV "+
			"tx-order resolver, the priceless-coverage SAC resolver and the supply "+
			"refresher's close-time reader must all dial through the retrying helper, or a "+
			"cold lake at boot disables them for the process lifetime again (K024)", c.viaGeneric, wantViaGenericRetry)
	}
	// Every component still allowed to dial the lake inline: the supply
	// refresher's close-time source, -dry-run branch ONLY (T184) — a
	// single synchronous dial so a bad config fails -dry-run instead of
	// only surfacing at a real start. The real boot path does not use
	// this call; it goes through newLazyCloseTimeReader (GH-902) above.
	knownDirectDials := []string{"NewExplorerReader"}
	if got := c.directConstructors(); !slices.Equal(got, knownDirectDials) {
		t.Errorf("direct lake dials in package main = %v, want %v:\n%s\na new one is a "+
			"component that gives up on a cold lake for its whole process lifetime; either "+
			"fail closed like the supply close-time reader or dial through "+
			"dialLakeReaderWithRetry, then account for it here",
			got, knownDirectDials, strings.Join(c.directSites(), "\n"))
	}
}

// The census is an instrument, so it is checked against a known case: a
// fixture package that dials the lake inline through an aliased import in
// a second file, via NewExplorerReaderAuth and NewTxIndexReader.
func TestLakeDialCensusSeesEveryInlineDial(t *testing.T) {
	ctors := lakeDialConstructors(t, clickhouseSourceDir)
	for _, name := range []string{"NewExplorerReader", "NewExplorerReaderAuth", "NewTxIndexReader", "NewSupplyReader"} {
		if !ctors[name] {
			t.Errorf("lake-dial constructor set is missing clickhouse.%s", name)
		}
	}
	if ctors["NewRefreshGate"] {
		t.Error("clickhouse.NewRefreshGate takes no context and dials nothing, but was counted as a lake dial")
	}

	c := censusLakeDials(t, filepath.Join("testdata", "lakedial"), ctors)
	want := []string{"NewExplorerReaderAuth", "NewTxIndexReader"}
	if got := c.directConstructors(); !slices.Equal(got, want) {
		t.Errorf("fixture direct lake dials = %v, want %v (sites: %v)", got, want, c.directSites())
	}
	if c.viaGeneric != 1 || c.viaDecimals != 0 {
		t.Errorf("fixture retrying dials = generic %d / decimals %d, want 1 / 0", c.viaGeneric, c.viaDecimals)
	}
}

const (
	clickhouseImportPath = "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	clickhouseSourceDir  = "../../internal/storage/clickhouse"
)

type lakeDial struct{ ctor, pos string }

type lakeDialCensus struct {
	direct                  []lakeDial
	viaDecimals, viaGeneric int
}

func (c lakeDialCensus) directConstructors() []string {
	out := make([]string, 0, len(c.direct))
	for _, d := range c.direct {
		out = append(out, d.ctor)
	}
	slices.Sort(out)
	return out
}

func (c lakeDialCensus) directSites() []string {
	out := make([]string, 0, len(c.direct))
	for _, d := range c.direct {
		out = append(out, "  clickhouse."+d.ctor+" at "+d.pos)
	}
	return out
}

// lakeDialConstructors returns the clickhouse package's exported
// constructors that take a context — the ones that open and ping a lake
// connection.
func lakeDialConstructors(t *testing.T, dir string) map[string]bool {
	t.Helper()
	ctors := map[string]bool{}
	forEachNonTestGoFile(t, dir, func(_ *token.FileSet, f *ast.File) {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Recv != nil || !fd.Name.IsExported() || !strings.HasPrefix(fd.Name.Name, "New") {
				continue
			}
			if params := fd.Type.Params.List; len(params) > 0 && isContextContext(params[0].Type) {
				ctors[fd.Name.Name] = true
			}
		}
	})
	if len(ctors) == 0 {
		t.Fatalf("no context-taking constructors found in %s — the census would match nothing", dir)
	}
	return ctors
}

func isContextContext(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "context" && sel.Sel.Name == "Context"
}

// censusLakeDials counts, across every non-test file in dir, the direct
// CALLS of a lake-dial constructor (a reference handed to a retrying
// helper is not a call) and the calls of the two retrying dial helpers.
func censusLakeDials(t *testing.T, dir string, ctors map[string]bool) lakeDialCensus {
	t.Helper()
	var c lakeDialCensus
	forEachNonTestGoFile(t, dir, func(fset *token.FileSet, f *ast.File) {
		local := clickhouseLocalName(t, f)
		ast.Inspect(f, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				c.record(fset, call, local, ctors)
			}
			return true
		})
	})
	return c
}

func (c *lakeDialCensus) record(fset *token.FileSet, call *ast.CallExpr, local string, ctors map[string]bool) {
	fun := call.Fun
	if idx, ok := fun.(*ast.IndexExpr); ok { // explicit instantiation: f[T](...)
		fun = idx.X
	}
	switch fn := fun.(type) {
	case *ast.SelectorExpr:
		if pkg, ok := fn.X.(*ast.Ident); ok && local != "" && pkg.Name == local && ctors[fn.Sel.Name] {
			c.direct = append(c.direct, lakeDial{ctor: fn.Sel.Name, pos: fset.Position(call.Pos()).String()})
		}
	case *ast.Ident:
		switch fn.Name {
		case "dialDecimalsResolver":
			c.viaDecimals++
		case "dialLakeReaderWithRetry":
			c.viaGeneric++
		}
	}
}

// clickhouseLocalName is the identifier f uses for the clickhouse package,
// or "" when f does not import it.
func clickhouseLocalName(t *testing.T, f *ast.File) string {
	t.Helper()
	for _, imp := range f.Imports {
		if path, err := strconv.Unquote(imp.Path.Value); err != nil || path != clickhouseImportPath {
			continue
		}
		if imp.Name == nil {
			return "clickhouse"
		}
		if imp.Name.Name == "." || imp.Name.Name == "_" {
			t.Fatalf("clickhouse imported as %q — the lake-dial census cannot see calls through it", imp.Name.Name)
		}
		return imp.Name.Name
	}
	return ""
}

func forEachNonTestGoFile(t *testing.T, dir string, fn func(*token.FileSet, *ast.File)) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	parsed := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		fn(fset, f)
		parsed++
	}
	if parsed == 0 {
		t.Fatalf("no non-test Go files in %s — the census scanned nothing", dir)
	}
}
