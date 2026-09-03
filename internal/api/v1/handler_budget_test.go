package v1

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/config"
)

// TestHandlerBudgets_StayInsideTheRequestTimeout is the structural guard
// behind the bodyless-200 class.
//
// A per-handler context.WithTimeout(r.Context(), d) earns its keep by
// surfacing a SPECIFIC 503 ("the pool's reserve state didn't decode in
// time") ahead of the blanket middleware.RequestTimeout deadline. A
// budget >= the global one inverts that: the middleware's deadline
// reaches the reader first, the handler's own timeout branch becomes
// unreachable, and the ceiling the code advertises is fiction. Live,
// /v1/lending/pools/{pool}/reserves declared 15s against a 15s global
// and answered a real pool with a bodyless HTTP 200 after 15.1s.
//
// Scanning the source is what makes the invariant enforceable: the
// budgets are duration literals and package constants inside handler
// bodies, so no runtime assertion can reach the ones a test never
// exercises. Every request-derived budget across the API packages has
// to hold, not just the handful with coverage.
//
// The bound is maxHandlerBudget rather than defaultRequestTimeout
// itself, because a budget landing in the last few hundred milliseconds
// before the blanket deadline has no room left to serialise and flush
// the 503 it exists to write.
//
// A budget the walker cannot EVALUATE fails too, naming the site: a
// silent skip is indistinguishable from a pass, and this guard exists
// precisely because an unguarded budget shipped.
func TestHandlerBudgets_StayInsideTheRequestTimeout(t *testing.T) {
	dirs := map[string][]string{}
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		dir := filepath.Dir(path)
		dirs[dir] = append(dirs[dir], path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	fset := token.NewFileSet()
	budgets := 0
	for _, paths := range dirs {
		files := make([]*ast.File, 0, len(paths))
		for _, path := range paths {
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			files = append(files, f)
		}
		// Constants resolve per directory: a budget is usually named
		// (maxHandlerBudget, explorerReadTimeout) rather than spelled
		// inline, and package scope is where those names are unambiguous.
		consts := durationConsts(files)
		for _, f := range files {
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || !isRequestWithTimeout(call) {
					return true
				}
				pos := fset.Position(call.Pos())
				d, ok := durationValue(call.Args[1], consts)
				if !ok {
					// A budget this walker cannot evaluate is a HOLE, not a
					// pass: skipping it silently is how a 30s literal spelled
					// in a form the resolver misses (a cross-package constant,
					// an arithmetic shape not handled below) would sail
					// through the guard reading green. Fail and name the site
					// so the choice is explicit — extend durationValue, or add
					// the site to budgetExemptions with a reason.
					if !budgetExempt(pos.Filename) {
						t.Errorf("%s: cannot evaluate the request-derived budget %s — extend durationValue "+
							"to resolve it, or exempt the site in budgetExemptions with a reason. An "+
							"unevaluable budget is unguarded, and this guard exists because an unguarded "+
							"one shipped at 15s against a 15s deadline",
							pos, exprString(call.Args[1]))
					}
					return true
				}
				budgets++
				if d > maxHandlerBudget {
					t.Errorf("%s: handler budget %s exceeds maxHandlerBudget %s (request timeout %s) — "+
						"the blanket deadline fires first, or leaves no room to serialise the response, "+
						"so this handler's own 503 branch is unreachable (name maxHandlerBudget for the "+
						"longest legal budget)",
						pos, d, maxHandlerBudget, defaultRequestTimeout)
				}
				return true
			})
		}
	}
	if budgets == 0 {
		t.Fatal("matched no context.WithTimeout(r.Context(), …) budget at all — the guard has stopped recognising the code it protects")
	}
}

// budgetExemptions names the files whose request-derived
// context.WithTimeout budget is legitimately not a handler budget, with
// the reason. Keyed on the path suffix so it survives a move within the
// tree.
//
// One entry only, and it should stay that way: every other site in the
// API tree is a handler capping one read.
var budgetExemptions = map[string]string{
	"middleware/request_timeout.go": "the blanket deadline ITSELF — its budget is the " +
		"configured api.request_timeout, supplied by the caller, so there is nothing static " +
		"to compare and nothing to compare it against",
}

func budgetExempt(filename string) bool {
	for suffix := range budgetExemptions {
		if strings.HasSuffix(filepath.ToSlash(filename), suffix) {
			return true
		}
	}
	return false
}

// exprString renders an unevaluable budget expression back to source so
// the failure message points at the actual spelling, not just a line.
func exprString(e ast.Expr) string {
	var buf strings.Builder
	if err := printer.Fprint(&buf, token.NewFileSet(), e); err != nil {
		return "<unprintable>"
	}
	return buf.String()
}

// TestMaxHandlerBudgetMatchesConfigBound ties this package's compile-time
// ceiling to the one api.request_timeout is validated against.
//
// maxHandlerBudget is derived from defaultRequestTimeout — the 15s
// FALLBACK. The deadline a deployment actually installs is
// Options.RequestTimeout, fed from the operator-settable
// api.request_timeout, and internal/config rejects a value that does not
// exceed [config.APIMaxHandlerBudget]. Two spellings of one number, so
// this asserts they are the same number: let them drift and
// api.request_timeout = 12s passes validation while every handler
// "capped" at 12s is back to a budget that can never fire —
// TestHandlerBudgets_StayInsideTheRequestTimeout staying green the whole
// time, because it only ever sees the constant.
func TestMaxHandlerBudgetMatchesConfigBound(t *testing.T) {
	if maxHandlerBudget != config.APIMaxHandlerBudget {
		t.Fatalf("maxHandlerBudget = %s but config.APIMaxHandlerBudget = %s — the boot-time "+
			"validation of api.request_timeout is no longer checking the bound this package enforces",
			maxHandlerBudget, config.APIMaxHandlerBudget)
	}
	if maxHandlerBudget >= defaultRequestTimeout {
		t.Fatalf("maxHandlerBudget %s >= defaultRequestTimeout %s — the longest legal handler budget "+
			"must leave room for the blanket deadline to be the backstop, not the first to fire",
			maxHandlerBudget, defaultRequestTimeout)
	}
}

// isRequestWithTimeout reports whether call is
// context.WithTimeout(r.Context(), …): a budget derived from the
// inbound request, as opposed to a background or store-scoped one that
// the request deadline does not bound.
func isRequestWithTimeout(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "WithTimeout" || len(call.Args) != 2 {
		return false
	}
	if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "context" {
		return false
	}
	inner, ok := call.Args[0].(*ast.CallExpr)
	if !ok {
		return false
	}
	innerSel, ok := inner.Fun.(*ast.SelectorExpr)
	if !ok || innerSel.Sel.Name != "Context" {
		return false
	}
	recv, ok := innerSel.X.(*ast.Ident)
	return ok && recv.Name == "r"
}

// durationConsts collects package-level `const name = <duration>`
// declarations so a named budget resolves to a number.
func durationConsts(files []*ast.File) map[string]time.Duration {
	out := map[string]time.Duration{}
	// Two passes: a constant may be defined in terms of an earlier one
	// (maxHandlerBudget derives from defaultRequestTimeout), and
	// declaration order across files is not guaranteed.
	for range 2 {
		for _, f := range files {
			for _, decl := range f.Decls {
				gen, ok := decl.(*ast.GenDecl)
				if !ok || gen.Tok != token.CONST {
					continue
				}
				for _, spec := range gen.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok || len(vs.Names) != 1 || len(vs.Values) != 1 {
						continue
					}
					if d, ok := durationValue(vs.Values[0], out); ok {
						out[vs.Names[0].Name] = d
					}
				}
			}
		}
	}
	return out
}

// durationValue evaluates the forms a budget is written in: `N *
// time.Unit`, a named constant, and a constant offset by a literal
// (`defaultRequestTimeout - 3*time.Second`). Anything else reports
// false rather than guessing.
func durationValue(e ast.Expr, consts map[string]time.Duration) (time.Duration, bool) {
	switch v := e.(type) {
	case *ast.Ident:
		d, ok := consts[v.Name]
		return d, ok
	case *ast.ParenExpr:
		return durationValue(v.X, consts)
	case *ast.BinaryExpr:
		if v.Op == token.MUL {
			return durationProduct(v, consts)
		}
		x, xok := durationValue(v.X, consts)
		y, yok := durationValue(v.Y, consts)
		if !xok || !yok {
			return 0, false
		}
		switch v.Op {
		case token.ADD:
			return x + y, true
		case token.SUB:
			return x - y, true
		default:
			// Any other operator is not a duration expression this walker
			// understands; the caller treats that as "cannot evaluate".
			return 0, false
		}
	}
	return 0, false
}

// durationProduct evaluates `N * time.Unit`, the spelling every inline
// budget uses.
func durationProduct(bin *ast.BinaryExpr, consts map[string]time.Duration) (time.Duration, bool) {
	lit, ok := bin.X.(*ast.BasicLit)
	if !ok || lit.Kind != token.INT {
		return 0, false
	}
	n, err := strconv.ParseInt(lit.Value, 10, 64)
	if err != nil {
		return 0, false
	}
	if unit, ok := timeUnit(bin.Y); ok {
		return time.Duration(n) * unit, true
	}
	if d, ok := durationValue(bin.Y, consts); ok {
		return time.Duration(n) * d, true
	}
	return 0, false
}

// timeUnit resolves a time.Millisecond / time.Second / time.Minute
// selector. Longer units are deliberately absent: a request-scoped
// budget measured in hours is a bug this guard should not normalise.
func timeUnit(e ast.Expr) (time.Duration, bool) {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return 0, false
	}
	if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "time" {
		return 0, false
	}
	switch sel.Sel.Name {
	case "Millisecond":
		return time.Millisecond, true
	case "Second":
		return time.Second, true
	case "Minute":
		return time.Minute, true
	}
	return 0, false
}
