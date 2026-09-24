package main

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

const offenderContract = "C_NONSTANDARD_DECIMALS_FIXTURE"

type toggleDecimalsReader struct{ err error }

func (r *toggleDecimalsReader) LoadNonstandardDecimalsAssets(context.Context) ([]timescale.NonstandardDecimalsAsset, error) {
	if r.err != nil {
		return nil, r.err
	}
	return []timescale.NonstandardDecimalsAsset{{Asset: offenderContract, Decimals: 6}}, nil
}

// Q198: the first load must complete before the prime returns, so nothing
// constructed after it can observe a cold cache.
func TestPrimeNonstandardDecimalsCache_LoadsBeforeReturning(t *testing.T) {
	cache := v1.NewNonstandardDecimalsCache(&toggleDecimalsReader{}, nil)
	check := primeNonstandardDecimalsCache(context.Background(), cache, discardLogger())

	if d, ok := cache.Lookup(offenderContract); !ok || d != 6 {
		t.Fatalf("Lookup after prime = (%d, %v), want (6, true)", d, ok)
	}
	if err := check.Ping(context.Background()); err != nil {
		t.Fatalf("Ping after successful prime: %v", err)
	}
	if !check.Critical() {
		t.Fatal("nonstandard-decimals readiness must be critical: a cold cache serves wrong prices")
	}
}

// Q198: a failed first load keeps the instance out of rotation until a load
// succeeds; a later failure keeps the last-good snapshot and stays ready.
func TestNonstandardDecimalsChecker_NotReadyUntilFirstLoad(t *testing.T) {
	reader := &toggleDecimalsReader{err: errors.New("pg down")}
	cache := v1.NewNonstandardDecimalsCache(reader, nil)
	check := primeNonstandardDecimalsCache(context.Background(), cache, discardLogger())

	if err := check.Ping(context.Background()); err == nil {
		t.Fatal("Ping on a never-loaded cache = nil, want not-ready")
	}

	reader.err = nil
	if err := cache.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if err := check.Ping(context.Background()); err != nil {
		t.Fatalf("Ping after first successful load: %v", err)
	}

	reader.err = errors.New("pg blip")
	_ = cache.Refresh(context.Background())
	if err := check.Ping(context.Background()); err != nil {
		t.Fatalf("Ping after a later failed refresh: %v (last-good must stay ready)", err)
	}
}

// TestRun_PrimesNonstandardDecimalsCacheBeforeServing pins the wiring for
// Q198: run() must prime the cache inline (not inside a goroutine), register
// the returned check in the readiness set before the server is built, and do
// so before any consumer of the cache is wired.
func TestRun_PrimesNonstandardDecimalsCacheBeforeServing(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	runBody := findFuncBody(file, "run")
	if runBody == nil {
		t.Fatal("run() not found in main.go")
	}

	var funcLits [][2]token.Pos
	var primePos, readyChecksPos token.Pos
	var cacheUses []token.Pos
	ast.Inspect(runBody, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncLit:
			funcLits = append(funcLits, [2]token.Pos{x.Pos(), x.End()})
		case *ast.AssignStmt:
			if p := primeAppendedToChecks(x); p.IsValid() {
				primePos = p
			}
		case *ast.KeyValueExpr:
			if k, ok := x.Key.(*ast.Ident); ok && k.Name == "ReadyChecks" && !readyChecksPos.IsValid() {
				readyChecksPos = x.Pos()
			}
		case *ast.Ident:
			if x.Name == "nonstandardDecimalsCache" {
				cacheUses = append(cacheUses, x.Pos())
			}
		}
		return true
	})

	if !primePos.IsValid() {
		t.Fatal("run() never does `checks = append(checks, primeNonstandardDecimalsCache(...))`: the cache's first load is not synchronised with serving")
	}
	for _, r := range funcLits {
		if primePos >= r[0] && primePos < r[1] {
			t.Fatalf("primeNonstandardDecimalsCache at %s runs inside a func literal; it must run inline", fset.Position(primePos))
		}
	}
	if !readyChecksPos.IsValid() || primePos > readyChecksPos {
		t.Fatalf("the nonstandard-decimals check is appended at %s, after ReadyChecks is handed to the server", fset.Position(primePos))
	}
	// Uses: [0] the declaration, [1] the prime argument; every later use is
	// a consumer and must come after the prime.
	if len(cacheUses) < 2 {
		t.Fatalf("expected the cache declaration and prime call, got %d uses", len(cacheUses))
	}
	for _, p := range cacheUses[1:] {
		if p < primePos {
			t.Fatalf("nonstandardDecimalsCache consumed at %s before it is primed", fset.Position(p))
		}
	}
}

func findFuncBody(file *ast.File, name string) *ast.BlockStmt {
	for _, d := range file.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == name {
			return fd.Body
		}
	}
	return nil
}

// primeAppendedToChecks returns the position of the prime call when a is
// `checks = append(checks, primeNonstandardDecimalsCache(...))`.
func primeAppendedToChecks(a *ast.AssignStmt) token.Pos {
	if len(a.Lhs) != 1 || len(a.Rhs) != 1 {
		return token.NoPos
	}
	if lhs, ok := a.Lhs[0].(*ast.Ident); !ok || lhs.Name != "checks" {
		return token.NoPos
	}
	call, ok := a.Rhs[0].(*ast.CallExpr)
	if !ok {
		return token.NoPos
	}
	if fn, ok := call.Fun.(*ast.Ident); !ok || fn.Name != "append" {
		return token.NoPos
	}
	for _, arg := range call.Args {
		inner, ok := arg.(*ast.CallExpr)
		if !ok {
			continue
		}
		if fn, ok := inner.Fun.(*ast.Ident); ok && fn.Name == "primeNonstandardDecimalsCache" {
			return inner.Pos()
		}
	}
	return token.NoPos
}
