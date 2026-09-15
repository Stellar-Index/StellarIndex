package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestShutdownSignalsTheStreamsAndNotEveryRequest guards the two halves
// of the 2026-09-15 deploy-stall fix that live in this file, both of
// which are one deleted line away from silently reverting.
//
// Half one: httpSrv.RegisterOnShutdown(apiSrv.BeginStreamDrain). Without
// it the stream writers never learn a drain started, and because
// http.Server.Shutdown waits for connections to go IDLE — which an SSE
// connection never does — one attached stream holds the listener for
// the entire 30s budget. Measured on r1: 30.18s with a browser on
// /v1/ledger/stream against 0.21s without one. Nothing else in the
// process fails when this line goes missing; the cost is paid only on a
// deploy that happens to have a viewer attached, which is why it needs
// a guard rather than a code review.
//
// Half two: the http.Server must NOT carry a BaseContext. Deriving
// request contexts from the process root context would also release the
// streams, and would cancel every ordinary in-flight request the moment
// SIGTERM landed — an abrupt teardown of the traffic that is not
// streaming, in exchange for tidying up the traffic that is. That is the
// obvious fix, and it is the wrong one.
//
// Asserted structurally because the wiring lives inline in run(), which
// needs a full dependency tree (Postgres, Redis, ClickHouse) to reach.
func TestShutdownSignalsTheStreamsAndNotEveryRequest(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	var registered int
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "RegisterOnShutdown" || len(call.Args) != 1 {
			return true
		}
		arg, ok := call.Args[0].(*ast.SelectorExpr)
		if ok && arg.Sel.Name == "BeginStreamDrain" {
			registered++
		}
		return true
	})

	if registered != 1 {
		t.Errorf("found %d RegisterOnShutdown(…BeginStreamDrain) call(s) in main.go, want exactly 1 — "+
			"without it an open SSE connection pins httpSrv.Shutdown for the whole "+
			"drain budget and the process exits on top of it", registered)
	}

	if pos := httpServerField(t, fset, file, "BaseContext"); pos != "" {
		t.Errorf("the API's http.Server sets BaseContext (%s). That cancels EVERY in-flight "+
			"request at SIGTERM, not just the streams — a graceful drain becomes an abrupt "+
			"one for the traffic that is not streaming. Signal the streams with the "+
			"RegisterOnShutdown drain instead", pos)
	}
	if pos := httpServerField(t, fset, file, "ConnContext"); pos != "" {
		t.Errorf("the API's http.Server sets ConnContext (%s), which has the same "+
			"cancel-everything effect as BaseContext at shutdown", pos)
	}
}

// httpServerField returns the source position of the named field if the
// http.Server composite literal in main.go sets it, or "" if it does
// not.
func httpServerField(t *testing.T, fset *token.FileSet, file *ast.File, name string) string {
	t.Helper()

	var found string
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := lit.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Server" {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "http" {
			return true
		}
		for _, elt := range lit.Elts {
			kv, isKV := elt.(*ast.KeyValueExpr)
			if !isKV {
				continue
			}
			key, isIdent := kv.Key.(*ast.Ident)
			if isIdent && key.Name == name {
				found = fset.Position(kv.Pos()).String()
			}
		}
		return true
	})
	return found
}
