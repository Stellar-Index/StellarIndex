package v1

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSSEProducerGoroutinesRecover is a guard-coverage test for AGT-12
// (audit-2026-07-24): every goroutine this package spawns MUST register a
// recover().
//
// Why this shape rather than a behavioural test. Detached goroutines here are
// started as bare `go s.someMethod(...)` calls or inline `go func(){...}()`
// literals. An unrecovered panic in ANY goroutine terminates the whole process
// — and `middleware.Recoverer` wraps only the handler goroutine, not these. The
// SSE streams among them are reachable unauthenticated, so a panic in the
// compute path is a remote crash of the API. A behavioural test would have to
// drive one goroutine to panic through a real dependency and wait a poll
// interval, and — critically — it would only ever cover the goroutines that
// exist today. The failure mode that actually bites is the next one someone
// adds without the guard. So this test derives the spawned set from the source
// itself and fails if any of their bodies lacks a recover(). That is the
// guard-coverage discipline: find every call site of the thing being guarded,
// not a name-matched sample.
//
// Earlier versions of this test restricted the `go s.<method>(...)` shape to
// methods whose name contained "StreamProducer" and ignored the
// `go func(){...}()` shape entirely. That missed forwardClosedStream and
// forwardTipStream (spawned the same way, named for what they forward rather
// than that they produce) and the price-tip shared-producer closure in
// price_tip_producers.go, which has no declaration to name-match at all.
//
// Proven red: deleting the deferred recover from any spawned method or
// literal in this package fails this test naming it (or, for a literal,
// naming its file).
func TestSSEProducerGoroutinesRecover(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	// funcBodies: every method on *Server declared in this package.
	funcBodies := map[string]*ast.FuncDecl{}
	// spawnedMethods: method names launched as `go s.name(...)`, with the file for context.
	spawnedMethods := map[string]string{}
	// spawnedLiterals: anonymous `go func(){...}()` goroutines, with the file for context.
	type spawnedLiteral struct {
		file string
		lit  *ast.FuncLit
	}
	var spawnedLiterals []spawnedLiteral

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch d := n.(type) {
			case *ast.FuncDecl:
				if d.Recv != nil && d.Name != nil {
					funcBodies[d.Name.Name] = d
				}
			case *ast.GoStmt:
				switch fn := d.Call.Fun.(type) {
				case *ast.SelectorExpr:
					// `go s.someMethod(...)`
					if ident, ok := fn.X.(*ast.Ident); ok && ident.Name == "s" {
						spawnedMethods[fn.Sel.Name] = name
					}
				case *ast.FuncLit:
					// `go func(...){...}(...)`
					spawnedLiterals = append(spawnedLiterals, spawnedLiteral{file: name, lit: fn})
				}
			}
			return true
		})
	}

	// Every method spawned as `go s.<method>(...)` is a detached goroutine and
	// needs its own recover — checking every one, not just names containing
	// "StreamProducer", is what catches forwardClosedStream/forwardTipStream.
	var checked []string
	for method, file := range spawnedMethods {
		decl, ok := funcBodies[method]
		if !ok {
			t.Errorf("%s: spawned as a goroutine in %s but its declaration was not found in this package", method, file)
			continue
		}
		if !blockRegistersRecover(decl.Body) {
			t.Errorf("%s (spawned as a bare goroutine in %s) does not register a recover(). "+
				"An unrecovered panic in a goroutine terminates the WHOLE process, and these "+
				"streams are reachable unauthenticated — add a deferred recover() that logs and "+
				"tears down only this connection (AGT-12).", method, file)
		}
		checked = append(checked, method)
	}

	// Anonymous `go func(){...}()` goroutines have no declaration to walk to,
	// so they are checked in place.
	var literalsChecked int
	for _, sc := range spawnedLiterals {
		if !blockRegistersRecover(sc.lit.Body) {
			t.Errorf("goroutine literal in %s does not register a recover(). "+
				"An unrecovered panic in a goroutine terminates the WHOLE process — add a "+
				"deferred recover() or worker.Recover() (AGT-12).", sc.file)
		}
		literalsChecked++
	}
	if literalsChecked == 0 {
		t.Errorf("expected to check at least one `go func(){...}()` literal — " +
			"the discovery in this test has drifted and is no longer protecting anything")
	}

	// Guard against the named-method check silently covering nothing (e.g. if
	// the spawn idiom changes and the AST match stops finding anything).
	for _, want := range []string{
		"runLedgerStreamProducer",
		"runTipStreamProducer",
		"runObservationsStreamProducer",
		"forwardClosedStream",
		"forwardTipStream",
	} {
		if !containsProducerName(checked, want) {
			t.Errorf("expected to check %s but it was not discovered as a spawned method — "+
				"the discovery in this test has drifted from the code and is no longer protecting anything", want)
		}
	}
}

// blockRegistersRecover reports whether body registers panic recovery via a
// top-level deferred statement: a func literal that calls recover() directly,
// a deferred call to the shared (*Server).recoverStreamProducer helper, or a
// deferred call to worker.Recover — all three ARE the deferred function, so
// recover() still fires in the panicking goroutine. All three are accepted so
// the guard survives whichever extraction a given call site uses without
// weakening what it asserts: the goroutine must register SOME recovery.
func blockRegistersRecover(body *ast.BlockStmt) bool {
	if body == nil {
		return false
	}
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		d, ok := n.(*ast.DeferStmt)
		if !ok {
			return true
		}
		ast.Inspect(d, func(m ast.Node) bool {
			call, ok := m.(*ast.CallExpr)
			if !ok {
				return true
			}
			if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "recover" {
				found = true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel != nil {
				pkg, ok := sel.X.(*ast.Ident)
				if sel.Sel.Name == "recoverStreamProducer" ||
					(ok && pkg.Name == "worker" && sel.Sel.Name == "Recover") {
					found = true
				}
			}
			return true
		})
		return true
	})
	return found
}

func containsProducerName(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
