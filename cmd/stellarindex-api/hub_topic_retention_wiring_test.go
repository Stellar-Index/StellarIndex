package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestHubTopicRetentionIsWiredFromConfig is the wiring guard for GH-1128:
// streaming.Hub.SetTopicIdleTTL / SetMaxTopics have existed on the Hub
// since it shipped — NewHub's own godoc names them as the override path —
// but a repo-wide grep found nothing non-test calling them, so every
// deployment silently ran the compiled-in defaults (idle TTL 15m, max
// topics 4096) with no operator lever, the same dead-knob shape
// AGT-dead-code (audit-2026-07-23) found and closed for
// streaming.SetMaxConcurrentStreams and T674 closed for the tip-producer
// ceiling.
//
// Read from the AST — like TestTipProducerCeilingIsWiredFromConfig in this
// package — because the wiring lives inside run()'s server construction,
// which needs a live Postgres to execute; the property to protect is that
// the calls exist and are fed from config, not the runtime effect.
func TestHubTopicRetentionIsWiredFromConfig(t *testing.T) {
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
		if !ok || sel.Sel.Name != "SetTopicIdleTTL" && sel.Sel.Name != "SetMaxTopics" {
			return true
		}
		if len(call.Args) != 1 {
			return true
		}
		if exprText(call.Args[0]) == wantHubArgFor(sel.Sel.Name) {
			got[sel.Sel.Name] = true
		}
		return true
	})

	for _, method := range []string{"SetTopicIdleTTL", "SetMaxTopics"} {
		if !got[method] {
			t.Errorf("main.go has no call hub.%s(%s) — the streaming Hub's topic retention is not wired from config (GH-1128)",
				method, wantHubArgFor(method))
		}
	}
}

// wantHubArgFor is the exact config selector each setter must be fed.
func wantHubArgFor(method string) string {
	if method == "SetTopicIdleTTL" {
		return "cfg.API.Streaming.TopicIdleTTL"
	}
	return "cfg.API.Streaming.MaxTopics"
}
