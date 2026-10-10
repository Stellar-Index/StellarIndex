// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// run() needs live Postgres/Redis/ClickHouse to execute, so wiring that
// must not regress is asserted on main.go's syntax tree. One row per rule.
type mainFile struct {
	fset *token.FileSet
	file *ast.File
}

// callsTo returns every call whose callee is a selector or ident named name.
func (m mainFile) callsTo(name string) []*ast.CallExpr {
	var out []*ast.CallExpr
	ast.Inspect(m.file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.SelectorExpr:
			if fn.Sel.Name == name {
				out = append(out, call)
			}
		case *ast.Ident:
			if fn.Name == name {
				out = append(out, call)
			}
		}
		return true
	})
	return out
}

// requireSetterFedBy fails unless some single-arg call to method is passed
// exactly the config selector want.
func requireSetterFedBy(t *testing.T, m mainFile, method, want, why string) {
	t.Helper()
	for _, call := range m.callsTo(method) {
		if len(call.Args) == 1 && exprText(call.Args[0]) == want {
			return
		}
	}
	t.Errorf("main.go has no call %s(%s): %s", method, want, why)
}

// serverLiteralField returns the position of the named field in an
// http.Server composite literal, or "" when no literal sets it.
func (m mainFile) serverLiteralField(name string) string {
	var found string
	ast.Inspect(m.file, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := lit.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Server" {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "http" {
			return true
		}
		for _, elt := range lit.Elts {
			if kv, ok := elt.(*ast.KeyValueExpr); ok {
				if key, ok := kv.Key.(*ast.Ident); ok && key.Name == name {
					found = m.fset.Position(kv.Pos()).String()
				}
			}
		}
		return true
	})
	return found
}

func TestMainWiringRules(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	m := mainFile{fset: fset, file: file}

	rules := []struct {
		name  string
		check func(t *testing.T)
	}{
		{
			// An unconditional key-cache invalidator DELs the canonical
			// credential under auth_backend=redis, destroying /v1/register keys.
			name: "api_key_budget_stores_via_backend_aware_constructor",
			check: func(t *testing.T) {
				for _, call := range m.callsTo("NewRedisKeyCacheInvalidator") {
					t.Errorf("main.go calls NewRedisKeyCacheInvalidator at %s; build the stores with v1.NewAPIKeyBudgetStores, which decides from auth_backend",
						fset.Position(call.Pos()))
				}
				ast.Inspect(file, func(n ast.Node) bool {
					assign, ok := n.(*ast.AssignStmt)
					if !ok {
						return true
					}
					for _, lhs := range assign.Lhs {
						sel, ok := lhs.(*ast.SelectorExpr)
						if !ok || sel.Sel.Name != "CacheInvalidator" {
							continue
						}
						if base, ok := sel.X.(*ast.Ident); ok && base.Name == "apiKeyBudgets" {
							t.Errorf("main.go assigns apiKeyBudgets.CacheInvalidator by hand at %s; it must come from v1.NewAPIKeyBudgetStores",
								fset.Position(assign.Pos()))
						}
					}
					return true
				})
				calls := m.callsTo("NewAPIKeyBudgetStores")
				if len(calls) != 1 {
					t.Fatalf("main.go calls NewAPIKeyBudgetStores %d times, want exactly 1", len(calls))
				}
				if len(calls[0].Args) != 3 {
					t.Fatalf("NewAPIKeyBudgetStores called with %d args, want 3", len(calls[0].Args))
				}
				if got := exprText(calls[0].Args[2]); got != "cfg.API.AuthBackend" {
					t.Errorf("NewAPIKeyBudgetStores is given auth backend %q, want cfg.API.AuthBackend — a literal re-opens the defect", got)
				}
			},
		},
		{
			// A conditional append could skip the checker; most CAGG readers
			// are correct only while every served view is materialized_only.
			name: "closed_bucket_checker_in_ready_checker_literal",
			check: func(t *testing.T) {
				found := 0
				ast.Inspect(file, func(n ast.Node) bool {
					lit, ok := n.(*ast.CompositeLit)
					if !ok {
						return true
					}
					arr, ok := lit.Type.(*ast.ArrayType)
					if !ok || arr.Len != nil {
						return true
					}
					if elt, ok := arr.Elt.(*ast.SelectorExpr); !ok || elt.Sel.Name != "ReadyChecker" {
						return true
					}
					for _, e := range lit.Elts {
						if call, ok := e.(*ast.CallExpr); ok {
							if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "NewClosedBucketChecker" {
								found++
							}
						}
					}
					return true
				})
				if found == 0 {
					t.Error("run() builds no v1.NewClosedBucketChecker into its []v1.ReadyChecker literal")
				}
			},
		},
		{
			name: "hub_topic_idle_ttl_from_config",
			check: func(t *testing.T) {
				requireSetterFedBy(t, m, "SetTopicIdleTTL", "cfg.API.Streaming.TopicIdleTTL", "streaming Hub topic retention is not wired from config")
			},
		},
		{
			name: "hub_max_topics_from_config",
			check: func(t *testing.T) {
				requireSetterFedBy(t, m, "SetMaxTopics", "cfg.API.Streaming.MaxTopics", "streaming Hub topic retention is not wired from config")
			},
		},
		{
			name: "tip_producer_ceiling_from_config",
			check: func(t *testing.T) {
				requireSetterFedBy(t, m, "SetMaxTipProducers", "cfg.API.Streaming.MaxTipProducers", "tip-producer ceiling is not wired from config")
			},
		},
		{
			name: "tip_producer_per_caller_ceiling_from_config",
			check: func(t *testing.T) {
				requireSetterFedBy(t, m, "SetMaxTipProducersPerCaller", "cfg.API.Streaming.MaxTipProducersPerCaller", "tip-producer ceiling is not wired from config")
			},
		},
		{
			// Without the drain hook one open SSE stream pins Shutdown for the
			// whole budget.
			name: "shutdown_registers_stream_drain_once",
			check: func(t *testing.T) {
				registered := 0
				for _, call := range m.callsTo("RegisterOnShutdown") {
					if len(call.Args) != 1 {
						continue
					}
					if arg, ok := call.Args[0].(*ast.SelectorExpr); ok && arg.Sel.Name == "BeginStreamDrain" {
						registered++
					}
				}
				if registered != 1 {
					t.Errorf("found %d RegisterOnShutdown(…BeginStreamDrain) calls in main.go, want exactly 1", registered)
				}
			},
		},
		{
			// A BaseContext cancels every in-flight request at SIGTERM, not
			// only the streams.
			name: "http_server_has_no_base_or_conn_context",
			check: func(t *testing.T) {
				for _, field := range []string{"BaseContext", "ConnContext"} {
					if pos := m.serverLiteralField(field); pos != "" {
						t.Errorf("the API's http.Server sets %s (%s); signal streams via the RegisterOnShutdown drain instead", field, pos)
					}
				}
			},
		},
	}
	for _, r := range rules {
		t.Run(r.name, r.check)
	}
}
