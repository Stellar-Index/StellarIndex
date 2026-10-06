package pricingguard

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// gateMethods are Gate's decision methods: asking one of these is what
// "consulted the withholding decision" means.
var gateMethods = map[string]bool{
	"PriceWithheld":         true,
	"PriceWithholding":      true,
	"PriceWithholdingAt":    true,
	"AssetValueWithholding": true,
	"Judge":                 true,
}

// halfMethods are the SubstanceGate / ScamGate decision methods. A binary
// that calls one folds the two halves itself, which is the per-binary
// drift Gate exists to prevent.
var halfMethods = map[string]bool{
	"Allowed":      true,
	"AllowedAt":    true,
	"Verdict":      true,
	"Probe":        true,
	"Measure":      true,
	"MeasureAt":    true,
	"Withheld":     true,
	"WithheldPair": true,
}

// closedVWAPSeamExempt lists the closed-bucket reads that cannot publish
// a gated value, keyed "<binary>:<Recv>.<Method>", each with its reason.
var closedVWAPSeamExempt = map[string]string{
	"stellarindex-api:storePriceReader.RecentClosedVWAP1mExists": "existence probe: returns a bool, never a price",
	"stellarindex-api:storeChange24hReader.USDPrice24hAgo": "gated upstream: populateChange24h returns early unless the " +
		"GATED lookupUSDPrice succeeds first",
}

// TestEveryBinaryAsksTheGate is the cross-binary seam guard. Each binary's
// own guard parses its own main.go, so no test could see a publish site
// in another binary or another file. This one walks the non-test sources
// of every cmd/* binary and fails when
//
//   - any function reads a closed VWAP bucket (a *ClosedVWAP* call on any
//     receiver) without asking a Gate, directly or through a package-level
//     function that does; or
//   - any call names a gate HALF's decision method, which folds the two
//     halves outside Gate.
func TestEveryBinaryAsksTheGate(t *testing.T) {
	binaries, err := filepath.Glob(filepath.Join("..", "..", "cmd", "*"))
	if err != nil {
		t.Fatalf("glob cmd/*: %v", err)
	}
	seams := map[string]int{}
	for _, dir := range binaries {
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			continue
		}
		bin := filepath.Base(dir)
		n, problems := gateSeamProblems(t, bin, dir)
		seams[bin] = n
		for _, p := range problems {
			t.Error(p)
		}
	}
	// A guard whose subject set is empty passes forever: both binaries
	// that publish aggregated prices must be seen reading closed buckets.
	for _, bin := range []string{"stellarindex-api", "stellarindex-aggregator"} {
		if seams[bin] == 0 {
			t.Errorf("found no closed-VWAP read seams in cmd/%s — the scan is broken, not the code clean", bin)
		}
	}
}

// gateSeamProblems parses one binary's non-test sources and returns its
// closed-VWAP seam count and every violation.
func gateSeamProblems(t *testing.T, bin, dir string) (int, []string) {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", dir, err)
	}
	var files []*ast.File
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			files = append(files, f)
		}
	}
	chokepoints := gateChokepoints(files)
	seams := 0
	var problems []string
	for _, f := range files {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			name := gateFuncName(fn)
			for _, sel := range halfReferences(fn.Body) {
				problems = append(problems, bin+": "+name+" calls ."+sel.Sel.Name+
					" at "+fset.Position(sel.Pos()).String()+" — a gate half consulted outside "+
					"pricingguard.Gate can withhold on one gate and forget the other")
			}
			if !readsClosedVWAPAnyReceiver(fn.Body) {
				continue
			}
			seams++
			if _, ok := closedVWAPSeamExempt[bin+":"+name]; ok {
				continue
			}
			if !asksGate(fn.Body, chokepoints) {
				problems = append(problems, bin+": "+name+" reads a closed VWAP bucket at "+
					fset.Position(fn.Pos()).String()+" without asking a pricingguard.Gate — "+
					"whatever consumes it would publish a price the gate refuses")
			}
		}
	}
	return seams, problems
}

// gateChokepoints returns the package-level functions whose body asks a
// Gate directly; a call to one counts as asking the Gate.
func gateChokepoints(files []*ast.File) map[string]bool {
	out := map[string]bool{}
	for _, f := range files {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Body == nil {
				continue
			}
			if asksGate(fn.Body, nil) {
				out[fn.Name.Name] = true
			}
		}
	}
	return out
}

func asksGate(body *ast.BlockStmt, chokepoints map[string]bool) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return !found
		}
		switch fun := call.Fun.(type) {
		case *ast.SelectorExpr:
			found = found || gateMethods[fun.Sel.Name]
		case *ast.Ident:
			found = found || chokepoints[fun.Name]
		}
		return !found
	})
	return found
}

// readsClosedVWAPAnyReceiver reports whether body references a
// *ClosedVWAP* method: a call, or a method value (f := r.db.X) that
// reads the same bucket when called.
func readsClosedVWAPAnyReceiver(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && strings.Contains(sel.Sel.Name, "ClosedVWAP") {
			found = true
		}
		return !found
	})
	return found
}

// gateHolderNames are the field names a SubstanceGate or ScamGate is held
// under across the binaries.
var gateHolderNames = map[string]bool{"substance": true, "Substance": true, "scam": true, "Scam": true}

// halfReferences returns every gate-half selector in body: each call on any
// receiver, and a method value taken from a gate holder or a local alias of
// one (g := r.scam; f := g.WithheldPair), which is a call deferred.
// Non-call references need a holder because the half names are generic:
// a rate-limit result carries a plain Allowed field.
func halfReferences(body *ast.BlockStmt) []*ast.SelectorExpr {
	calls := map[*ast.SelectorExpr]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				calls[sel] = true
			}
		}
		return true
	})
	aliases := gateHolderAliases(body)
	var out []*ast.SelectorExpr
	ast.Inspect(body, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if ok && halfMethods[sel.Sel.Name] && (calls[sel] || isGateHolder(sel.X, aliases)) {
			out = append(out, sel)
		}
		return true
	})
	return out
}

// gateHolderAliases returns the locals body binds to a gate holder.
func gateHolderAliases(body *ast.BlockStmt) map[string]bool {
	aliases := map[string]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		var lhs []*ast.Ident
		var rhs []ast.Expr
		switch st := n.(type) {
		case *ast.AssignStmt:
			for _, e := range st.Lhs {
				id, _ := e.(*ast.Ident)
				lhs = append(lhs, id)
			}
			rhs = st.Rhs
		case *ast.ValueSpec:
			lhs, rhs = st.Names, st.Values
		}
		if len(lhs) != len(rhs) {
			return true
		}
		for i, id := range lhs {
			if id != nil && isGateHolder(rhs[i], nil) {
				aliases[id.Name] = true
			}
		}
		return true
	})
	return aliases
}

// isGateHolder reports whether expr names a gate: a holder field
// (s.substance, r.Scam), a bare holder parameter, or a local alias of one.
func isGateHolder(expr ast.Expr, aliases map[string]bool) bool {
	switch x := expr.(type) {
	case *ast.SelectorExpr:
		return gateHolderNames[x.Sel.Name]
	case *ast.Ident:
		return gateHolderNames[x.Name] || aliases[x.Name]
	case *ast.ParenExpr:
		return isGateHolder(x.X, aliases)
	}
	return false
}

func gateFuncName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	recv := fn.Recv.List[0].Type
	if star, ok := recv.(*ast.StarExpr); ok {
		recv = star.X
	}
	if id, ok := recv.(*ast.Ident); ok {
		return id.Name + "." + fn.Name.Name
	}
	return fn.Name.Name
}

// TestGateSeamGuardCatchesEachShape proves the guard is not vacuous: each
// synthetic binary below carries a violation shape the guard
// must name.
func TestGateSeamGuardCatchesEachShape(t *testing.T) {
	cases := map[string]string{
		"ungated seam on a non-s receiver": `package main
func (r reader) Latest() { r.db.LatestClosedVWAP1mForPair(nil, nil) }`,
		"half folded by hand": `package main
func (r reader) Latest() {
	r.db.LatestClosedVWAP1mForPair(nil, nil)
	_ = r.scam.WithheldPair(nil, a, b, "x") || !r.substance.Allowed(nil, a, b, "x")
}`,
		"ungated seam through a method value": `package main
func (r reader) Latest() { f := r.db.LatestClosedVWAP1mForPair; f(nil, nil) }`,
		"ungated seam through a store alias": `package main
func (r reader) Latest() { st := r.db; st.LatestClosedVWAP1mForPair(nil, nil) }`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o600); err != nil {
				t.Fatal(err)
			}
			n, problems := gateSeamProblems(t, "synthetic", dir)
			if n != 1 || len(problems) == 0 {
				t.Fatalf("seams=%d problems=%v — the guard missed this shape", n, problems)
			}
		})
	}
	t.Run("gated through a package-level chokepoint", func(t *testing.T) {
		dir := t.TempDir()
		src := `package main
func withheld(g Gate) bool { return g.PriceWithheld(nil, a, b, "x") }
func (r reader) Latest() { r.db.LatestClosedVWAP1mForPair(nil, nil); _ = withheld(r.g) }`
		if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
		n, problems := gateSeamProblems(t, "synthetic", dir)
		if n != 1 || len(problems) != 0 {
			t.Fatalf("seams=%d problems=%v, want one gated seam", n, problems)
		}
	})
}

// TestGateHalfScanCatchesMethodValues: a half folded outside Gate is named
// however it is spelled, with no closed-bucket read to make the case fail
// for another reason, and a non-gate Allowed field is left alone.
func TestGateHalfScanCatchesMethodValues(t *testing.T) {
	cases := []struct {
		name, src string
		want      int
	}{
		{"direct call", `func (r reader) ok() bool { return r.scam.WithheldPair(nil, a, b, "x") }`, 1},
		{"method value", `func (r reader) ok() bool { f := r.scam.WithheldPair; return f(nil, a, b, "x") }`, 1},
		{"method value through a gate alias", `func (r reader) ok() bool { g := r.substance; return apply(g.Allowed) }`, 1},
		{"a non-gate Allowed field", `func (r reader) ok() bool { res := r.limiter.Take(); return res.Allowed }`, 0},
	}
	for _, c := range cases {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"+c.src), 0o600); err != nil {
			t.Fatal(err)
		}
		n, problems := gateSeamProblems(t, "synthetic", dir)
		if n != 0 || len(problems) != c.want {
			t.Errorf("%s: seams=%d problems=%v, want 0 seams and %d problems", c.name, n, problems, c.want)
		}
	}
}
