// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
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
		{name: "clickhouse_check_registered_on_failed_dial", check: ruleClickhouseCheckRegisteredOnFailedDial},
		{name: "nonstandard_decimals_refresh_not_fatal", check: ruleNonstandardDecimalsRefreshNotFatal},
		{name: "signup_reaper_binds_to_postgres_store", check: ruleSignupReaperBindsToPostgresStore},
		{name: "dex_tvl_gate_via_nilable_builder", check: ruleDEXTVLGateViaNilAbleBuilder},
		{name: "nonstandard_decimals_primed_before_serving", check: ruleNonstandardDecimalsPrimedBeforeServing},
		{name: "lake_reader_dials_use_boot_retry", check: ruleLakeReaderDialsUseBootRetry},
		{name: "change24h_never_returns_raw_bucket", check: ruleChange24hNeverReturnsRawBucket},
		{name: "change24h_wired_with_decimals_table", check: ruleChange24hWiredWithDecimalsTable},
	}
	for _, r := range rules {
		t.Run(r.name, r.check)
	}
}

// TestRun_RegistersTheClickhouseCheckOnTheFailedDialPathToo is the
// wiring guard that a ClickHouse readiness checker must be
// registered on a path that a FAILED boot dial also takes.
//
// The defect it pins is positional, not logical — `checks = append(
// checks, clickhouseChecker{r: er})` sat inside the `else` of the dial's
// error check, which is the branch a failed dial does not take. So a
// ClickHouse that was already down when the API started published no
// `stellarindex_dependency_up{dependency="clickhouse"}` series at all,
// the `== 0` alert (deliberately not absent(), per its own in-file
// rationale) had nothing to match, and the state the annotation calls
// "the only signal that it is gone" was the state with no signal.
//
// The checker is built in the wiring package and registered from run(),
// so both halves are checked: the wiring package constructs a
// ClickhouseChecker outside an else-branch, and run() appends
// wiring.ClickhouseReadyChecks to its checks outside one.
//
// Read from the AST so the assertion is about the code rather than
// about a list someone has to remember to update.
func ruleClickhouseCheckRegisteredOnFailedDial(t *testing.T) {
	fset := token.NewFileSet()
	checkers, err := parser.ParseFile(fset, filepath.Join(wiringDir, "checkers.go"), nil, 0)
	if err != nil {
		t.Fatalf("parse %s/checkers.go: %v", wiringDir, err)
	}
	constructed, constructedInElse := countOutsideElse(checkers, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return false
		}
		id, ok := lit.Type.(*ast.Ident)
		return ok && id.Name == "ClickhouseChecker"
	})
	if constructed == 0 {
		t.Fatal("the wiring package constructs no ClickhouseChecker — ClickHouse has no readiness checker at all, so stellarindex_dependency_up{dependency=\"clickhouse\"} is never published")
	}
	if constructedInElse == constructed {
		t.Errorf("every ClickhouseChecker in the wiring package (%d) is constructed inside an else-branch — a boot dial that FAILS takes the if-branch, so the one state the dependency gauge exists to report is the one state it is absent for", constructed)
	}

	mainFile, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	run := findFuncBody(mainFile, "run")
	if run == nil {
		t.Fatal("run() not found in main.go")
	}
	registered, registeredInElse := countOutsideElse(run, isClickhouseChecksAppend)
	if registered == 0 {
		t.Fatal("run() never does `checks = append(checks, wiring.ClickhouseReadyChecks(...)...)` — the checker is built but never registered, so stellarindex_dependency_up{dependency=\"clickhouse\"} is never published")
	}
	if registeredInElse == registered {
		t.Errorf("every wiring.ClickhouseReadyChecks registration in run() (%d) is inside an else-branch — a boot dial that FAILS takes the if-branch, so the one state the dependency gauge exists to report is the one state it is absent for", registered)
	}
}

// countOutsideElse counts the nodes under root that match, and how many
// of them sit inside an else-branch.
func countOutsideElse(root ast.Node, match func(ast.Node) bool) (total, inElse int) {
	var elseRanges [][2]token.Pos
	ast.Inspect(root, func(n ast.Node) bool {
		if ifs, ok := n.(*ast.IfStmt); ok && ifs.Else != nil {
			elseRanges = append(elseRanges, [2]token.Pos{ifs.Else.Pos(), ifs.Else.End()})
		}
		return true
	})
	ast.Inspect(root, func(n ast.Node) bool {
		if n == nil || !match(n) {
			return true
		}
		total++
		for _, r := range elseRanges {
			if n.Pos() >= r[0] && n.Pos() < r[1] {
				inElse++
				break
			}
		}
		return true
	})
	return total, inElse
}

// isClickhouseChecksAppend matches
// `checks = append(checks, wiring.ClickhouseReadyChecks(...)...)`.
func isClickhouseChecksAppend(n ast.Node) bool {
	a, ok := n.(*ast.AssignStmt)
	if !ok || len(a.Lhs) != 1 || len(a.Rhs) != 1 {
		return false
	}
	if lhs, ok := a.Lhs[0].(*ast.Ident); !ok || lhs.Name != "checks" {
		return false
	}
	call, ok := a.Rhs[0].(*ast.CallExpr)
	if !ok {
		return false
	}
	if fn, ok := call.Fun.(*ast.Ident); !ok || fn.Name != "append" {
		return false
	}
	for _, arg := range call.Args {
		inner, ok := arg.(*ast.CallExpr)
		if !ok {
			continue
		}
		sel, ok := inner.Fun.(*ast.SelectorExpr)
		if !ok {
			continue
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "wiring" && sel.Sel.Name == "ClickhouseReadyChecks" {
			return true
		}
	}
	return false
}

// Source-level tripwire, the API twin of
// the aggregator rule decimals_refresh_is_fatal — but the two
// have DIFFERENT verdicts. The aggregator's boot refresh is fatal: it has no readiness surface between it and the orchestrator loop
// that publishes prices. The API sits behind `/v1/readyz`, so a fatal-on-boot
// load (make run() return an error) is replaced by a synchronous prime +
// critical readiness check
// (wiring.PrimeNonstandardDecimalsCache, see nonstandard_decimals_ready_test.go):
// a failed first load now keeps the process up but red, instead of
// putting systemd into a restart loop on a Postgres blip.
//
// This guards the other direction: run() must NOT reintroduce a direct,
// fatal `nonstandardDecimalsCache.Refresh` call, since that would bring
// the restart-loop failure mode back.

func ruleNonstandardDecimalsRefreshNotFatal(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	var run *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == "run" {
			run = fd
		}
	}
	if run == nil || run.Body == nil {
		t.Fatal("no top-level run() in main.go — re-derive where the API boots")
	}

	ast.Inspect(run.Body, func(n ast.Node) bool {
		// A refresh inside a goroutine or closure is the periodic loop —
		// its Warn-and-continue is fine and out of scope here.
		if _, ok := n.(*ast.FuncLit); ok {
			return false
		}
		ifStmt, ok := n.(*ast.IfStmt)
		if !ok || !initCallsRefreshOn(ifStmt, "nonstandardDecimalsCache") {
			return true
		}
		if blockReturns(ifStmt.Body) {
			t.Errorf("main.go:%d — run() aborts startup directly on a failed "+
				"nonstandardDecimalsCache.Refresh again. That was reverted (RLT-366 -> Q198): "+
				"it puts systemd into a restart loop on a Postgres blip. Cold-cache safety "+
				"belongs in the critical readiness check (wiring.PrimeNonstandardDecimalsCache), not "+
				"a fatal return here.", fset.Position(ifStmt.Pos()).Line)
		}
		return true
	})
}

// initCallsRefreshOn matches `if err := <recv>.Refresh(ctx); err != nil`.
func initCallsRefreshOn(ifStmt *ast.IfStmt, recv string) bool {
	assign, ok := ifStmt.Init.(*ast.AssignStmt)
	if !ok || len(assign.Rhs) != 1 {
		return false
	}
	call, ok := assign.Rhs[0].(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Refresh" {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == recv
}

// blockReturns reports whether a block returns at its own statement level.
func blockReturns(body *ast.BlockStmt) bool {
	for _, stmt := range body.List {
		if _, ok := stmt.(*ast.ReturnStmt); ok {
			return true
		}
	}
	return false
}

// TestRun_SignupReaperBindsToThePostgresAccountStoreNotTheDashboardBundle
// pins NS02: POST /v1/register (ADR-0049) is wired onto
// platformAccountStore/registerAccountStore whenever Postgres is
// reachable — independent of api.dashboard.base_url. The reaper that
// cleans up its signup-race orphans must bind to that same seam. Asserting
// the OrphanStore type against dashboardBundle.accounts instead means a
// Postgres-only, no-dashboard deployment (the shipped example.toml default:
// no [api.dashboard] base_url) accepts registrations but never runs the
// reaper, because buildDashboardBundle returns the zero value when
// BaseURL == "".
func ruleSignupReaperBindsToPostgresStore(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	var assertedOn string
	ast.Inspect(file, func(n ast.Node) bool {
		ta, ok := n.(*ast.TypeAssertExpr)
		if !ok {
			return true
		}
		sel, ok := ta.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "OrphanStore" {
			return true
		}
		switch x := ta.X.(type) {
		case *ast.Ident:
			assertedOn = x.Name
		case *ast.SelectorExpr:
			if base, ok := x.X.(*ast.Ident); ok {
				assertedOn = base.Name + "." + x.Sel.Name
			} else {
				assertedOn = x.Sel.Name
			}
		}
		return true
	})

	if assertedOn == "" {
		t.Fatal("run() asserts no value against signupreaper.OrphanStore — the reaper wiring moved or was removed")
	}
	if assertedOn != "platformAccountStore" {
		t.Fatalf("signup-reaper's OrphanStore assertion binds to %q, want %q (the same store POST /v1/register uses); "+
			"binding it to dashboardBundle.accounts instead leaves the reaper unwired in a Postgres-only, "+
			"no-dashboard deployment even though registration is active there", assertedOn, "platformAccountStore")
	}
}

// TestDEXTVLGateIsWiredThroughTheNilAbleBuilder is the WIRING half.
//
// v1.DEXTVLSources.Gate is an interface, and an interface holding a
// non-pointer struct is never == nil however empty the struct is. Assigning
// `dexTVLValueGate{…}` as a composite literal unconditionally would make v1's
// nil-Gate arm — the one that keeps Basis quiet about screens that did not
// run — unreachable in this binary, leaving
// TestDEXTVLCache_NoGateKeepsTodaysFigures over in internal/api/v1 pinning a
// path production never takes.
//
// An AST guard rather than a behavioural one because the wiring lives
// inside main()'s server construction, which needs a live Postgres to
// run: the property to protect is that the assignment goes through the
// nil-able builder at all. Same shape as TestPriceServingSeamsAreGated
// in this package.
func ruleDEXTVLGateViaNilAbleBuilder(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := lit.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "DEXTVLSources" {
			return true
		}
		for _, elt := range lit.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok || key.Name != "Gate" {
				continue
			}
			found = true
			call, ok := kv.Value.(*ast.CallExpr)
			if !ok {
				t.Errorf("DEXTVLSources.Gate is assigned %T at %s; it must come from "+
					"buildDEXTVLValueGate, which returns a nil interface when no guard is wired",
					kv.Value, fset.Position(kv.Value.Pos()))
				continue
			}
			fn, ok := call.Fun.(*ast.Ident)
			if !ok || fn.Name != "buildDEXTVLValueGate" {
				t.Errorf("DEXTVLSources.Gate is built by %s at %s, want buildDEXTVLValueGate",
					exprText(call.Fun), fset.Position(call.Pos()))
			}
		}
		return true
	})
	if !found {
		t.Fatal("no v1.DEXTVLSources literal with a Gate field in main.go — " +
			"the TVL snapshot lost its serving trust gate (#338), or this guard needs re-aiming")
	}
}

// TestRun_PrimesNonstandardDecimalsCacheBeforeServing pins the wiring for
// Q198: run() must prime the cache inline (not inside a goroutine), register
// the returned check in the readiness set before the server is built, and do
// so before any consumer of the cache is wired.
func ruleNonstandardDecimalsPrimedBeforeServing(t *testing.T) {
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
		t.Fatal("run() never does `checks = append(checks, wiring.PrimeNonstandardDecimalsCache(...))`: the cache's first load is not synchronised with serving")
	}
	for _, r := range funcLits {
		if primePos >= r[0] && primePos < r[1] {
			t.Fatalf("wiring.PrimeNonstandardDecimalsCache at %s runs inside a func literal; it must run inline", fset.Position(primePos))
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
// `checks = append(checks, wiring.PrimeNonstandardDecimalsCache(...))`.
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
		fn, ok := inner.Fun.(*ast.SelectorExpr)
		if !ok {
			continue
		}
		if pkg, ok := fn.X.(*ast.Ident); ok && pkg.Name == "wiring" && fn.Sel.Name == "PrimeNonstandardDecimalsCache" {
			return inner.Pos()
		}
	}
	return token.NoPos
}

// Every lake reader dial in main.go must go through the boot retry:
// a bare one-shot dial is the cold-boot race that latches the checker down.
func ruleLakeReaderDialsUseBootRetry(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	var retryRanges [][2]token.Pos
	var dials []*ast.CallExpr
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			if fn.Name == "dialClickHouseAtBoot" || fn.Name == "dialLakeReadersAtBoot" {
				retryRanges = append(retryRanges, [2]token.Pos{call.Pos(), call.End()})
			}
		case *ast.SelectorExpr:
			if pkg, ok := fn.X.(*ast.Ident); ok && pkg.Name == "clickhouse" &&
				(fn.Sel.Name == "NewExplorerReaderAuth" || fn.Sel.Name == "NewSupplyReaderAuth") {
				dials = append(dials, call)
			}
		}
		return true
	})
	if len(dials) == 0 {
		t.Fatal("main.go dials no lake reader; this guard is looking at the wrong names")
	}
	for _, d := range dials {
		inside := false
		for _, r := range retryRanges {
			if d.Pos() >= r[0] && d.End() <= r[1] {
				inside = true
			}
		}
		if !inside {
			sel, _ := d.Fun.(*ast.SelectorExpr)
			t.Errorf("clickhouse.%s is dialled once outside dialLakeReadersAtBoot/dialClickHouseAtBoot — a ClickHouse a few seconds late at boot leaves its seams nil and the readiness gauge at 0 until restart", sel.Sel.Name)
		}
	}
}

// Every bucket USDPrice24hAgo hands back must have gone through the
// normaliser: a `return row.VWAP, nil` is the defect.
func ruleChange24hNeverReturnsRawBucket(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	fn := findMethod(f, "storeChange24hReader", "USDPrice24hAgo")
	if fn == nil {
		t.Fatal("could not locate storeChange24hReader.USDPrice24hAgo in main.go — " +
			"update this guard to follow the refactor rather than deleting it")
	}
	normalised, reads := 0, 0
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.CallExpr:
			if id, ok := v.Fun.(*ast.Ident); ok && id.Name == "normalizeChange24hAnchor" {
				normalised++
			}
			if sel, ok := v.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "ClosedVWAP1mAtOrBefore" {
				reads++
			}
		case *ast.ReturnStmt:
			if len(v.Results) == 0 {
				return true
			}
			if sel, ok := v.Results[0].(*ast.SelectorExpr); ok && sel.Sel.Name == "VWAP" {
				t.Errorf("USDPrice24hAgo returns a raw %s.VWAP at %s — the anchor must be "+
					"decimals-normalised like the current price it is divided into",
					exprName(sel.X), fset.Position(v.Pos()))
			}
		}
		return true
	})
	if reads == 0 {
		t.Fatal("found no ClosedVWAP1mAtOrBefore read in USDPrice24hAgo — the scan is broken")
	}
	if normalised < reads {
		t.Errorf("USDPrice24hAgo makes %d bucket reads but normalises only %d — "+
			"every read path (the peg fallback included) must normalise against the pair it read",
			reads, normalised)
	}
}

// A normaliser handed a nil table is a byte-identical no-op, so the
// wiring is what makes the fix live: main() must pass the cache.
func ruleChange24hWiredWithDecimalsTable(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	literals := 0
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		if id, ok := lit.Type.(*ast.Ident); !ok || id.Name != "storeChange24hReader" {
			return true
		}
		literals++
		wired := false
		for _, el := range lit.Elts {
			kv, ok := el.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "decimals" {
				if v, ok := kv.Value.(*ast.Ident); ok && v.Name != "nil" {
					wired = true
				}
			}
		}
		if !wired {
			t.Errorf("storeChange24hReader constructed at %s without a decimals table — "+
				"the anchor normalisation is inert and change_24h_pct mixes scales again",
				fset.Position(lit.Pos()))
		}
		return true
	})
	if literals == 0 {
		t.Fatal("found no storeChange24hReader literal in main.go — the scan is broken")
	}
}

func exprName(e ast.Expr) string {
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return "<expr>"
}
