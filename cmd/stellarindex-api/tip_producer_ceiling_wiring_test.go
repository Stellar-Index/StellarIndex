package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestTipProducerCeilingIsWiredFromConfig is the wiring guard for T674:
// v1.Server.SetMaxTipProducers and SetMaxTipProducersPerCaller have
// existed on the shared tip-stream producer registry since it shipped
// (RT-1 / UNAUTH-DOS-1), but a repo-wide grep found nothing calling
// them — every deployment silently ran the package-level hardcoded
// defaults (512 / 24) with no operator lever to tune or disable the
// ceiling, the same dead-knob shape AGT-dead-code (audit-2026-07-23)
// found and closed for streaming.SetMaxConcurrentStreams.
//
// Read from the AST — like TestDEXTVLGateIsWiredThroughTheNilAbleBuilder
// and TestRun_RegistersTheClickhouseCheckOnTheFailedDialPathToo in this
// package — because the wiring lives inside run()'s server construction,
// which needs a live Postgres to execute; the property to protect is
// that the call exists and is fed from config, not the runtime effect.
func TestTipProducerCeilingIsWiredFromConfig(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	got := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "SetMaxTipProducers" && sel.Sel.Name != "SetMaxTipProducersPerCaller" {
			return true
		}
		if len(call.Args) != 1 {
			return true
		}
		if exprText(call.Args[0]) == wantArgFor(sel.Sel.Name) {
			got[sel.Sel.Name] = true
		}
		return true
	})

	for _, method := range []string{"SetMaxTipProducers", "SetMaxTipProducersPerCaller"} {
		if !got[method] {
			t.Errorf("main.go has no call apiSrv.%s(%s) — the tip-producer ceiling is not wired from config (T674)",
				method, wantArgFor(method))
		}
	}
}

// wantArgFor is the exact config selector each setter must be fed.
func wantArgFor(method string) string {
	if method == "SetMaxTipProducers" {
		return "cfg.API.Streaming.MaxTipProducers"
	}
	return "cfg.API.Streaming.MaxTipProducersPerCaller"
}
