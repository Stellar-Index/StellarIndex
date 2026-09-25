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

// TestPriceAtAndPriceChangesHaveRequestBudgets pins RLT-455 directly
// against the two files it named, rather than relying on the general
// walker above (which only validates budgets that EXIST and is
// structurally blind to a handler with none at all). price_at.go's
// alias walk and price_changes.go's up-to-five sequential PriceAt
// calls (current anchor + 4 horizons) had no per-request
// context.WithTimeout, so a slow run held its pool connection until
// the client gave up with nothing here to catch it.
func TestPriceAtAndPriceChangesHaveRequestBudgets(t *testing.T) {
	for _, file := range []string{"price_at.go", "price_changes.go"} {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		found := false
		ast.Inspect(f, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok && isRequestWithTimeout(call) {
				found = true
			}
			return true
		})
		if !found {
			t.Errorf("%s: no context.WithTimeout(r.Context(), …) budget — an unbounded "+
				"handler read holds its pool connection until the client gives up", file)
		}
	}
}

// TestRWAHistoryBudgetsStayInsideTheRequestTimeout pins RLT-043 directly
// against rwaHistoryBudget and rwaPremiumHistoryBudget, rather than
// relying on the general walker above. Neither is spelled as a literal
// context.WithTimeout(r.Context(), …): cachedRWAValueHistory and
// cachedRWAPremiumHistory take a plain ctx parameter because they are
// also invoked from the background prewarm in rwa.go, so the walker's
// receiver-name check correctly does not match the call inside
// buildRWAValueHistory / buildRWAPremiumHistory. Both are nonetheless
// reachable synchronously off the request context — handleRWAHistory
// calls cachedRWAValueHistory(r.Context()), whose leader branch runs
// buildRWAValueHistory(ctx) inline on a cache miss — so they owe the
// same ceiling every other handler budget does.
func TestRWAHistoryBudgetsStayInsideTheRequestTimeout(t *testing.T) {
	if rwaHistoryBudget > maxHandlerBudget {
		t.Errorf("rwaHistoryBudget %s exceeds maxHandlerBudget %s (request timeout %s) — "+
			"reachable synchronously from handleRWAHistory on a cache miss, so the blanket "+
			"deadline fires first and this handler's own unavailable-response branch is unreachable",
			rwaHistoryBudget, maxHandlerBudget, defaultRequestTimeout)
	}
	if rwaPremiumHistoryBudget > maxHandlerBudget {
		t.Errorf("rwaPremiumHistoryBudget %s exceeds maxHandlerBudget %s (request timeout %s) — "+
			"same reachability as rwaHistoryBudget, via handleRWAPremiumHistory",
			rwaPremiumHistoryBudget, maxHandlerBudget, defaultRequestTimeout)
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

// apiSource is one parsed non-test file of the API tree, with the
// duration constants of its package directory.
type apiSource struct {
	path   string
	file   *ast.File
	consts map[string]time.Duration
}

func parseAPISources(t *testing.T, fset *token.FileSet) []apiSource {
	t.Helper()
	dirs := map[string][]*ast.File{}
	paths := map[*ast.File]string{}
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		dirs[filepath.Dir(path)] = append(dirs[filepath.Dir(path)], f)
		paths[f] = filepath.ToSlash(path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	var out []apiSource
	for _, files := range dirs {
		consts := durationConsts(files)
		for _, f := range files {
			out = append(out, apiSource{path: paths[f], file: f, consts: consts})
		}
	}
	return out
}

// isContextCall reports whether e is a call to context.<name>(…).
func isContextCall(e ast.Expr, name string) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "context"
}

// siteKey names a function for the exemption maps below: its file path
// and its own name. Exemptions are suffix-matched against it, so they
// survive a move within the tree.
func siteKey(path string, fd *ast.FuncDecl) string {
	return path + "#" + fd.Name.Name
}

func matchSite(exemptions map[string]string, key string) (string, bool) {
	for k := range exemptions {
		if strings.HasSuffix(key, k) {
			return k, true
		}
	}
	return "", false
}

// funcDecls calls fn for every function with a body in the API tree.
func funcDecls(t *testing.T, fset *token.FileSet, fn func(src apiSource, fd *ast.FuncDecl)) {
	t.Helper()
	for _, src := range parseAPISources(t, fset) {
		for _, decl := range src.file.Decls {
			if fd, ok := decl.(*ast.FuncDecl); ok && fd.Body != nil {
				fn(src, fd)
			}
		}
	}
}

// inlineDetachments names every function that detaches a context with
// context.WithoutCancel on the goroutine it runs on, rather than inside a
// `go` statement, and why the time that adds is acceptable. A detached
// context is invisible to the request deadline, so time spent under one
// on a request goroutine is time no handler budget and no
// api.request_timeout accounts for. A detached cache build belongs on its
// own goroutine; an entry here is a deliberate, bounded exception.
var inlineDetachments = map[string]string{
	"register.go#mintRegisterKey": "error path only: rolls back a mirror write the failed " +
		"request left behind, which must not fail on that request's dead context; 5s",
	"register.go#suspendRegisterOrphan": "error path only: quarantines an orphaned account " +
		"before the 500; 5s",
	"rwa_curated.go#rwaCuratedSnapshotWithin": "the curated snapshot refresh on /v1/rwa/assets, " +
		"at most once per rwaCuratedSnapshotTTL and bounded by rwaCuratedReadBudget (5s). The " +
		"inline-build shape the contract-protocol index used to have: a known residual, not a pattern to copy",
	"dashboardauth/middleware.go#newTouchCtx": "constructor for the context the async " +
		"TouchSession goroutine runs under; never used on the request goroutine",
	"middleware/ratelimit.go#throttleContext": "the rate-limit take runs before the handler " +
		"and must count an aborted request; bounded by throttleTakeTimeout",
	"middleware/usage.go#UsageTracker": "post-response usage metering, which must count an " +
		"aborted request; bounded by postResponseWriteTimeout",
	"middleware/touch_usage.go#TouchUsage": "post-response key touch; bounded by postResponseWriteTimeout",
	"explorer/operations.go#stampTxOutcomes": "the tx-outcome stitch keeps request values " +
		"without the request's cancellation; bounded by txOutcomeStitchBudget (1s)",
}

// TestDetachedContextsLeaveTheRequestGoroutine covers what the budget
// walker above cannot see: context.WithTimeout(context.WithoutCancel(ctx), d)
// is not a request-derived budget, so that walker never evaluates it, yet
// on a request goroutine its d is added to whatever the handler already
// spent. The contract-protocol index built that way on the cohort request
// path — its 5s on top of the 8s explorer read budget, past the ceiling.
//
// Every context.WithoutCancel in the API tree must sit inside a `go`
// statement (the call or its arguments), or its function must be named
// in inlineDetachments with a reason.
func TestDetachedContextsLeaveTheRequestGoroutine(t *testing.T) {
	fset := token.NewFileSet()
	seen, used := 0, map[string]bool{}
	funcDecls(t, fset, func(src apiSource, fd *ast.FuncDecl) {
		var goStmts []*ast.GoStmt
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if g, ok := n.(*ast.GoStmt); ok {
				goStmts = append(goStmts, g)
			}
			return true
		})
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			e, ok := n.(ast.Expr)
			if !ok || !isContextCall(e, "WithoutCancel") {
				return true
			}
			seen++
			for _, g := range goStmts {
				if e.Pos() >= g.Pos() && e.End() <= g.End() {
					return true
				}
			}
			key := siteKey(src.path, fd)
			if k, ok := matchSite(inlineDetachments, key); ok {
				used[k] = true
				return true
			}
			t.Errorf("%s: context.WithoutCancel on the calling goroutine — time spent under it is "+
				"invisible to the request deadline. Run the detached work in a `go` statement, or name "+
				"%q in inlineDetachments with why the time it adds is acceptable",
				fset.Position(e.Pos()), key)
			return true
		})
	})
	if seen == 0 {
		t.Fatal("found no context.WithoutCancel at all — the guard has stopped recognising the code it protects")
	}
	for k := range inlineDetachments {
		if !used[k] {
			t.Errorf("inlineDetachments[%q] matches no inline context.WithoutCancel — remove the stale exemption", k)
		}
	}
}

// longBudgets names every function whose context.WithTimeout exceeds
// maxHandlerBudget on a context that is not visibly detached, and why no
// request goroutine ever runs it.
var longBudgets = map[string]string{
	"rwa_contracts.go#buildRWADirectoryMembership": "membership rebuild, run only on the " +
		"refreshRWAMembership goroutine that readRWAMembership starts",
	"rwa_contracts.go#buildRWAListingMembership": "membership rebuild, run only on the " +
		"refreshRWAMembership goroutine that readRWAMembership starts",
	"liquidity_pools.go#PrewarmNativeLiquidityPools": "prewarm, called only from the " +
		"stellarindex-api prewarm loop on rootCtx",
	"sdex_orderbook.go#MaintainTick": "order-book maintenance, called only from the " +
		"stellarindex-api maintenance loop on rootCtx",
	"sdex_orderbook.go#loadWithin": "the order-book load, reached only through MaintainTick",
}

// TestLongBudgetsAreDetached closes the other half of the walker's blind
// spot: it matches context.WithTimeout(r.Context(), d) literally, so a
// budget on a ctx PARAMETER a handler passes r.Context() into is never
// checked. /v1/rwa/assets valued its contract rows under a 20s budget
// that way, past the 15s blanket deadline, so a cut valuation could only
// ever be served as if it were a refusal.
//
// A budget above maxHandlerBudget must hang off context.Background(),
// context.TODO() or context.WithoutCancel(…), or its function must be
// named in longBudgets with the reason no request goroutine runs it.
func TestLongBudgetsAreDetached(t *testing.T) {
	fset := token.NewFileSet()
	used := map[string]bool{}
	funcDecls(t, fset, func(src apiSource, fd *ast.FuncDecl) {
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || !isContextCall(call, "WithTimeout") || len(call.Args) != 2 {
				return true
			}
			d, ok := durationValue(call.Args[1], src.consts)
			if !ok || d <= maxHandlerBudget {
				return true
			}
			parent := call.Args[0]
			if isContextCall(parent, "Background") || isContextCall(parent, "TODO") ||
				isContextCall(parent, "WithoutCancel") {
				return true
			}
			key := siteKey(src.path, fd)
			if k, ok := matchSite(longBudgets, key); ok {
				used[k] = true
				return true
			}
			t.Errorf("%s: budget %s exceeds maxHandlerBudget %s on %s, which a handler can pass "+
				"r.Context() into — give the request path its own budget within the ceiling, detach "+
				"this one explicitly, or name %q in longBudgets with why no request runs it",
				fset.Position(call.Pos()), d, maxHandlerBudget, exprString(parent), key)
			return true
		})
	})
	for k := range longBudgets {
		if !used[k] {
			t.Errorf("longBudgets[%q] matches no long budget — remove the stale exemption", k)
		}
	}
}
