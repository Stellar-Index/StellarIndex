package main

// Source-level tripwire (the internal/api/v1/slo_guard_test.go pattern):
// the freeze WRITER must never again be gated on the Phase 1 anomaly
// checker.
//
// 2026-08-22, r1 XLM/GBP incident: the writer was built under
// `if checker != nil && rdb != nil`, but the Phase 2 confidence
// lifecycle (orchestrator.stepPhase2Freeze) runs on every scored bucket
// regardless of cfg.Anomaly and REFUSES publication when its 3-signal
// AND fires. A Phase-1-off / [anomaly.phase2]-tuned deployment (exactly
// r1's TOML) therefore engaged real freezes that wrote NO Redis marker:
// the frozen 5m/1h windows kept serving their last value with
// flags.frozen absent — a stale price presented as fresh, the precise
// state the marker exists to prevent.
//
// The guard pins the property, not a spelling: no conditional that
// lexically encloses freeze.NewWriter (if/else-if arm, switch, loop), and
// no earlier early-exit `if`, may read the value buildAnomalyChecker
// returns, any variable derived from it, or the config expression it was
// built from. It fails closed when the construction cannot be located.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"strings"
	"testing"
)

func TestFreezeWriterNotGatedOnPhase1Checker(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	violations, err := freezeWriterGateViolations(src)
	if err != nil {
		t.Fatalf("%v — update this guard to follow the refactor rather than deleting it", err)
	}
	for _, v := range violations {
		t.Errorf("freeze.NewWriter construction is gated on the Phase 1 anomaly checker (%s) — "+
			"Phase 2 freezes fire with Anomaly nil and would refuse publication with no "+
			"Redis marker (stale price served with flags.frozen absent; r1 XLM/GBP 2026-08-22)", v)
	}
}

// TestFreezeWriterGuardCatchesEveryGateShape proves the guard above is not
// vacuous: each gated shape must be reported and each ungated one must not.
func TestFreezeWriterGuardCatchesEveryGateShape(t *testing.T) {
	const pre = "package main\nfunc realMain() error {\n" +
		"\tchecker, err := buildAnomalyChecker(cfg.Anomaly)\n" +
		"\tif err != nil {\n\t\treturn err\n\t}\n"
	const post = "\treturn nil\n}\n"
	cases := []struct {
		name  string
		body  string
		gated bool
	}{
		{"current shape", "\tif rdb != nil {\n\t\tw, _ := freeze.NewWriter(rdb, 0)\n\t\t_ = w\n" +
			"\t} else if checker != nil {\n\t\tlog()\n\t}\n", false},
		{"hoisted out of every conditional", "\tw, _ := freeze.NewWriter(rdb, 0)\n\t_ = w\n", false},
		{"original incident", "\tif checker != nil && rdb != nil {\n\t\tfreeze.NewWriter(rdb, 0)\n\t}\n", true},
		{"alias of checker", "\tphase1 := checker\n\tif rdb != nil && phase1 != nil {\n" +
			"\t\tfreeze.NewWriter(rdb, 0)\n\t}\n", true},
		{"var alias of checker", "\tvar on = checker != nil\n\tif on {\n\t\tfreeze.NewWriter(rdb, 0)\n\t}\n", true},
		{"else arm of a checker if", "\tif checker == nil {\n\t\tlog()\n\t} else if rdb != nil {\n" +
			"\t\tfreeze.NewWriter(rdb, 0)\n\t}\n", true},
		{"early return on checker", "\tif checker == nil {\n\t\treturn nil\n\t}\n" +
			"\tfreeze.NewWriter(rdb, 0)\n", true},
		{"phase 1 config gate", "\tif cfg.Anomaly.Enabled && rdb != nil {\n\t\tfreeze.NewWriter(rdb, 0)\n\t}\n", true},
		{"switch on checker", "\tswitch {\n\tcase checker != nil:\n\t\tfreeze.NewWriter(rdb, 0)\n\t}\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, err := freezeWriterGateViolations([]byte(pre + tc.body + post))
			if err != nil {
				t.Fatalf("guard could not analyse the fixture: %v", err)
			}
			if got := len(v) > 0; got != tc.gated {
				t.Errorf("gated = %v (violations %q), want %v", got, v, tc.gated)
			}
		})
	}

	for name, body := range map[string]string{
		"writer absent":        "\tlog()\n",
		"writer in a closure":  "\tbuild := func() {\n\t\tfreeze.NewWriter(rdb, 0)\n\t}\n\tif checker != nil {\n\t\tbuild()\n\t}\n",
		"writer twice":         "\tfreeze.NewWriter(rdb, 0)\n\tfreeze.NewWriter(rdb, 0)\n",
		"checker never called": "",
	} {
		src := pre + body + post
		if name == "checker never called" {
			src = "package main\nfunc realMain() {\n\tfreeze.NewWriter(rdb, 0)\n}\n"
		}
		if _, err := freezeWriterGateViolations([]byte(src)); err == nil {
			t.Errorf("%s: guard did not fail closed", name)
		}
	}
}

// freezeWriterGateViolations returns one entry per conditional that gates
// freeze.NewWriter on the Phase 1 checker, or an error when the construction
// cannot be located unambiguously (fail closed).
func freezeWriterGateViolations(src []byte) ([]string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", src, 0)
	if err != nil {
		return nil, err
	}
	call, path, err := fwLocateNewWriter(f)
	if err != nil {
		return nil, err
	}
	fn, ok := path[0].(*ast.FuncDecl)
	if !ok {
		return nil, fmt.Errorf("freeze.NewWriter is not inside a top-level function")
	}
	taint := fwSeedTaint(fn)
	if taint.empty() {
		return nil, fmt.Errorf("freeze.NewWriter's function %s does not call buildAnomalyChecker", fn.Name.Name)
	}
	taint.propagate(fn.Body)

	var out []string
	report := func(n ast.Node, exprs ...ast.Node) {
		for _, e := range exprs {
			if taint.reads(e) {
				out = append(out, fset.Position(n.Pos()).String())
				return
			}
		}
	}
	for _, n := range path {
		switch s := n.(type) {
		case *ast.IfStmt:
			report(s, s.Init, s.Cond)
		case *ast.SwitchStmt:
			report(s, s.Init, s.Tag)
		case *ast.CaseClause:
			for _, e := range s.List {
				report(s, e)
			}
		case *ast.ForStmt:
			report(s, s.Init, s.Cond)
		case *ast.RangeStmt:
			report(s, s.X)
		}
	}
	// An earlier `if <checker…> { return }` dominates the construction as
	// surely as an enclosing arm does.
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		s, ok := n.(*ast.IfStmt)
		if ok && s.End() < call.Pos() && fwExits(s.Body) {
			report(s, s.Init, s.Cond)
		}
		return true
	})
	return out, nil
}

// fwLocateNewWriter finds the single freeze.NewWriter call and the chain of
// its ancestors, outermost (the FuncDecl) first.
func fwLocateNewWriter(f *ast.File) (*ast.CallExpr, []ast.Node, error) {
	var (
		stack []ast.Node
		found []ast.Node
		call  *ast.CallExpr
		count int
	)
	ast.Inspect(f, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		if c, ok := n.(*ast.CallExpr); ok && fwIsSelector(c.Fun, "freeze", "NewWriter") {
			count++
			call = c
			found = append([]ast.Node(nil), stack...)
		}
		stack = append(stack, n)
		return true
	})
	if count != 1 {
		return nil, nil, fmt.Errorf("found %d freeze.NewWriter calls in main.go, want exactly 1", count)
	}
	for i, n := range found {
		if _, ok := n.(*ast.FuncDecl); ok {
			found = found[i:]
			break
		}
	}
	for _, n := range found {
		if _, ok := n.(*ast.FuncLit); ok {
			return nil, nil, fmt.Errorf("freeze.NewWriter moved into a closure; its call sites are not analysed")
		}
	}
	return call, found, nil
}

func fwIsSelector(e ast.Expr, pkg, name string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == pkg && sel.Sel.Name == name
}

func fwExits(b *ast.BlockStmt) bool {
	exits := false
	ast.Inspect(b, func(n ast.Node) bool {
		switch n.(type) {
		case *ast.ReturnStmt, *ast.BranchStmt:
			exits = true
		}
		return !exits
	})
	return exits
}

// fwTaint is the set of names (and config expressions) that carry the Phase 1
// checker's value.
type fwTaint struct {
	names map[string]bool
	exprs []string
}

func (t *fwTaint) empty() bool { return len(t.names) == 0 }

// fwSeedTaint taints the checker returned by buildAnomalyChecker and the
// config expression it was built from.
func fwSeedTaint(fn *ast.FuncDecl) *fwTaint {
	t := &fwTaint{names: map[string]bool{}}
	fwEachBinding(fn.Body, func(lhs []*ast.Ident, rhs []ast.Expr) {
		if len(rhs) != 1 || len(lhs) == 0 {
			return
		}
		c, ok := rhs[0].(*ast.CallExpr)
		if !ok {
			return
		}
		if id, ok := c.Fun.(*ast.Ident); !ok || id.Name != "buildAnomalyChecker" {
			return
		}
		t.names[lhs[0].Name] = true
		for _, a := range c.Args {
			t.exprs = append(t.exprs, types.ExprString(a))
		}
	})
	return t
}

// propagate taints every variable assigned from a tainted expression, to a
// fixed point. `err` is exempt so the checker's own error check is not a gate.
func (t *fwTaint) propagate(body *ast.BlockStmt) {
	for changed := true; changed; {
		changed = false
		fwEachBinding(body, func(lhs []*ast.Ident, rhs []ast.Expr) {
			hit := false
			for _, r := range rhs {
				hit = hit || t.reads(r)
			}
			for _, id := range lhs {
				if hit && id.Name != "_" && id.Name != "err" && !t.names[id.Name] {
					t.names[id.Name] = true
					changed = true
				}
			}
		})
	}
}

func (t *fwTaint) reads(n ast.Node) bool {
	if n == nil {
		return false
	}
	hit := false
	ast.Inspect(n, func(m ast.Node) bool {
		switch e := m.(type) {
		case *ast.Ident:
			hit = hit || t.names[e.Name]
		case *ast.SelectorExpr:
			s := types.ExprString(e)
			for _, x := range t.exprs {
				hit = hit || s == x || strings.HasPrefix(s, x+".")
			}
		}
		return !hit
	})
	return hit
}

// fwEachBinding visits every `:=`/`=` assignment and `var` spec in body.
func fwEachBinding(body *ast.BlockStmt, visit func(lhs []*ast.Ident, rhs []ast.Expr)) {
	ast.Inspect(body, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.AssignStmt:
			var lhs []*ast.Ident
			for _, l := range s.Lhs {
				if id, ok := l.(*ast.Ident); ok {
					lhs = append(lhs, id)
				}
			}
			visit(lhs, s.Rhs)
		case *ast.ValueSpec:
			visit(s.Names, s.Values)
		}
		return true
	})
}
