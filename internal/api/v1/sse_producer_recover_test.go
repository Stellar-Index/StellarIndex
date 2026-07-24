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
// (audit-2026-07-24): every SSE producer that runs in its own goroutine MUST
// register a recover().
//
// Why this shape rather than a behavioural test. These producers are started as
// bare `go s.runXStreamProducer(...)` goroutines. An unrecovered panic in ANY
// goroutine terminates the whole process — and `middleware.Recoverer` wraps only
// the handler goroutine, not these. The streams are reachable unauthenticated, so
// a panic in the compute path is a remote crash of the API. A behavioural test
// would have to drive one producer to panic through a real dependency and wait a
// poll interval, and — critically — it would only ever cover the producers that
// exist today. The failure mode that actually bites is the FOURTH stream someone
// adds next year without the guard. So this test derives the producer set from
// the source itself (every `go s.<name>(...)` call in this package) and fails if
// any of their bodies lacks a recover(). That is the guard-coverage discipline:
// find every call site of the thing being guarded, not a sample.
//
// Proven red: deleting the `defer func(){ recover() ... }()` from any one
// producer fails this test naming that producer.
func TestSSEProducerGoroutinesRecover(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	// funcBodies: every method on *Server declared in this package.
	funcBodies := map[string]*ast.FuncDecl{}
	// spawned: method names launched as `go s.name(...)`, with the file for context.
	spawned := map[string]string{}

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
				// `go s.someMethod(...)`
				if call, ok := d.Call.Fun.(*ast.SelectorExpr); ok {
					if ident, ok := call.X.(*ast.Ident); ok && ident.Name == "s" {
						spawned[call.Sel.Name] = name
					}
				}
			}
			return true
		})
	}

	// Restrict to the SSE producer family. Naming is the contract here: these are
	// the long-lived per-connection goroutines. If a producer is ever named
	// differently, the assertion below that we found the known ones will catch the
	// drift.
	var checked []string
	for method, file := range spawned {
		if !strings.Contains(method, "StreamProducer") {
			continue
		}
		decl, ok := funcBodies[method]
		if !ok {
			t.Errorf("%s: spawned as a goroutine in %s but its declaration was not found in this package", method, file)
			continue
		}
		if !bodyRegistersRecover(decl) {
			t.Errorf("%s (spawned as a bare goroutine in %s) does not register a recover(). "+
				"An unrecovered panic in a goroutine terminates the WHOLE process, and these "+
				"streams are reachable unauthenticated — add a deferred recover() that logs and "+
				"tears down only this connection (AGT-12).", method, file)
		}
		checked = append(checked, method)
	}

	// Guard against the test silently covering nothing (e.g. if the spawn idiom
	// changes and the AST match stops finding anything).
	for _, want := range []string{
		"runLedgerStreamProducer",
		"runTipStreamProducer",
		"runObservationsStreamProducer",
	} {
		if !containsProducerName(checked, want) {
			t.Errorf("expected to check %s but it was not discovered as a spawned producer — "+
				"the discovery in this test has drifted from the code and is no longer protecting anything", want)
		}
	}
}

// bodyRegistersRecover reports whether fn's body contains a deferred call whose
// body invokes recover().
func bodyRegistersRecover(fn *ast.FuncDecl) bool {
	if fn.Body == nil {
		return false
	}
	found := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
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
