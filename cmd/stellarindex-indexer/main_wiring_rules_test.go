// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// Wiring in main.go that no behavioural test can reach (run() needs live
// dependencies) is asserted on its syntax tree. One row per rule.
func TestMainWiringRules(t *testing.T) {
	for _, r := range []struct {
		name  string
		check func(t *testing.T)
	}{
		{name: "ch_live_sink_dial_nonfatal_and_retried", check: ruleCHLiveSinkDialNonFatalAndRetried},
		{name: "dry_run_exits_before_background_workers", check: ruleDryRunExitsBeforeBackgroundWorkers},
		{name: "run_gates_phase4_on_cagg_coverage", check: ruleRunGatesPhase4OnCAGGCoverage},
		{name: "shutdown_select_never_returns_before_drain", check: ruleShutdownSelectNeverReturnsBeforeTheDrain},
		{name: "indexer_acts_on_sink_shutdown_loss", check: ruleIndexerActsOnSinkShutdownLoss},
	} {
		t.Run(r.name, r.check)
	}
}

// TestCHLiveSinkDialIsNonFatalAndRetried is a structural guard.
//
// clickhouse.NewLiveSink must not be dialled once, inline, in realMain: a
// single failure returned a fatal boot error, so a host whose ClickHouse
// was still loading metadata after a shared reboot — the same cold-boot
// race startSignerTagger's docstring describes — could not ingest a
// single ledger until an operator noticed and restarted the indexer. The
// dial must instead go through startCHLiveSink, which retries in its own
// goroutine and never blocks or fails realMain.
//
// Asserted structurally, like TestShutdownSelectNeverReturnsBeforeTheDrain
// in shutdown_drain_test.go: the wiring lives inline in realMain, which
// needs a full dependency tree (config, Postgres, Redis) to run.
func ruleCHLiveSinkDialNonFatalAndRetried(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	var directDials, viaHelper int
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.SelectorExpr:
			// A CALL of clickhouse.NewLiveSink — not a reference to it.
			pkg, pkgOK := fn.X.(*ast.Ident)
			if pkgOK && pkg.Name == "clickhouse" && fn.Sel != nil && fn.Sel.Name == "NewLiveSink" {
				directDials++
			}
		case *ast.Ident:
			if fn.Name == "startCHLiveSink" {
				viaHelper++
			}
		}
		return true
	})

	// The one dial that must exist is the retried one inside
	// startCHLiveSink itself; realMain must not have a second, bare one.
	if directDials != 1 {
		t.Errorf("found %d clickhouse.NewLiveSink call(s) in main.go, want exactly 1 (inside "+
			"startCHLiveSink's retry loop) — a second, bare call in realMain would fail indexer "+
			"boot on a cold ClickHouse again (K024)", directDials)
	}
	if viaHelper != 1 {
		t.Errorf("found %d startCHLiveSink call(s) in main.go, want exactly 1 — the ClickHouse "+
			"real-time dual-sink dial must go through the retrying helper", viaHelper)
	}

	// No fatal `return fmt.Errorf(...)` inside the ClickHouseLiveSink boot
	// branch: that shape is what made a cold ClickHouse fatal to indexer
	// boot rather than merely delaying the sink.
	ast.Inspect(file, func(n ast.Node) bool {
		ifStmt, ok := n.(*ast.IfStmt)
		if !ok {
			return true
		}
		src := nodeText(t, fset, ifStmt)
		if !strings.Contains(src, "ClickHouseLiveSink") {
			return true
		}
		if strings.Contains(src, "return fmt.Errorf") {
			t.Errorf("the ClickHouseLiveSink boot branch at %s contains a fatal `return fmt.Errorf` "+
				"— a ClickHouse that has not answered yet must delay the live sink, not fail indexer "+
				"boot (K024)", fset.Position(ifStmt.Pos()))
		}
		return true
	})
}

// TestDryRunExitsBeforeBackgroundWorkers pins that `-dry-run` ("load config +
// open connections + exit without ingesting") returns from run() before any
// background worker is launched. The routed-via and signer taggers issue
// UPDATEs against trades on their immediate first sweep and again on
// shutdown, and the decoder-stats flusher writes on its shutdown drain, so a
// launcher that precedes the dry-run return turns a config check into a
// production write.
func ruleDryRunExitsBeforeBackgroundWorkers(t *testing.T) {
	fset := token.NewFileSet()
	body := runBody(t, fset)

	exit := -1
	for i, stmt := range body.List {
		if ifs, ok := stmt.(*ast.IfStmt); ok {
			if id, ok := ifs.Cond.(*ast.Ident); ok && id.Name == "dryRun" {
				exit = i
				break
			}
		}
	}
	if exit < 0 {
		t.Fatal("run() has no top-level `if dryRun { ... }` exit")
	}

	for _, stmt := range body.List[:exit] {
		ast.Inspect(stmt, func(n ast.Node) bool {
			if launch := backgroundLaunch(n); launch != "" {
				t.Errorf("run() launches %s at %s, before the dry-run exit; "+
					"move it below `if dryRun { return nil }`", launch, fset.Position(n.Pos()))
			}
			return true
		})
	}

	// Non-vacuity: the launchers this test exists for must still be called
	// by run(), after the exit. A rename that drops one out of the scan
	// fails here instead of passing silently.
	after := map[string]bool{}
	for _, stmt := range body.List[exit+1:] {
		ast.Inspect(stmt, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if id, ok := call.Fun.(*ast.Ident); ok {
					after[id.Name] = true
				}
			}
			return true
		})
	}
	for _, want := range []string{"startRoutedViaTagger", "startSignerTagger", "startCHLiveSink", "watchPostgresPing"} {
		if !after[want] {
			t.Errorf("run() no longer calls %s after the dry-run exit; update this guard's launcher list", want)
		}
	}
}

// backgroundLaunch names n if it starts background work: a `go` statement, a
// package-local start*/watch* launcher, or a .Start()/.Run() method call.
func backgroundLaunch(n ast.Node) string {
	switch v := n.(type) {
	case *ast.GoStmt:
		return "a goroutine"
	case *ast.CallExpr:
		switch fun := v.Fun.(type) {
		case *ast.Ident:
			if strings.HasPrefix(fun.Name, "start") || strings.HasPrefix(fun.Name, "watch") {
				return fun.Name + "()"
			}
		case *ast.SelectorExpr:
			if fun.Sel.Name == "Start" || fun.Sel.Name == "Run" {
				return "." + fun.Sel.Name + "()"
			}
		}
	}
	return ""
}

func runBody(t *testing.T, fset *token.FileSet) *ast.BlockStmt {
	t.Helper()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == "run" {
			return fn.Body
		}
	}
	t.Fatal("main.go has no func run")
	return nil
}

// TestRunGatesPhase4OnCAGGCoverage pins that run() passes the selected sink
// mode through pipeline.VerifySoleWriterCAGGCoverage, and returns its error,
// before the sink goroutine starts — otherwise persist_per_source = false
// could be deployed on projector-lag evidence alone.
func ruleRunGatesPhase4OnCAGGCoverage(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var run *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "run" && fd.Recv == nil {
			run = fd
		}
	}
	if run == nil {
		t.Fatal("main.go has no func run")
	}
	var gate, persist token.Pos
	gateReturned := false
	ast.Inspect(run.Body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.IfStmt:
			if call := assignedCall(n.Init); isPipelineCall(call, "VerifySoleWriterCAGGCoverage") {
				gate = call.Pos()
				last, _ := call.Args[len(call.Args)-1].(*ast.Ident)
				if last == nil || last.Name != "sinkMode" {
					t.Errorf("VerifySoleWriterCAGGCoverage is not passed sinkMode")
				}
				gateReturned = len(n.Body.List) > 0 && isReturn(n.Body.List[0])
			}
		case *ast.CallExpr:
			if isPipelineCall(n, "PersistEvents") && persist == token.NoPos {
				persist = n.Pos()
			}
		}
		return true
	})
	switch {
	case gate == token.NoPos:
		t.Fatal("run() never calls pipeline.VerifySoleWriterCAGGCoverage in an if-init")
	case !gateReturned:
		t.Error("run() does not return when pipeline.VerifySoleWriterCAGGCoverage fails")
	case persist == token.NoPos || persist < gate:
		t.Errorf("pipeline.VerifySoleWriterCAGGCoverage (%s) must run before pipeline.PersistEvents (%s)",
			fset.Position(gate), fset.Position(persist))
	}
}

func assignedCall(s ast.Stmt) *ast.CallExpr {
	as, ok := s.(*ast.AssignStmt)
	if !ok || len(as.Rhs) != 1 {
		return nil
	}
	call, _ := as.Rhs[0].(*ast.CallExpr)
	return call
}

func isPipelineCall(call *ast.CallExpr, name string) bool {
	if call == nil {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "pipeline"
}

func isReturn(s ast.Stmt) bool {
	_, ok := s.(*ast.ReturnStmt)
	return ok
}

// TestShutdownSelectNeverReturnsBeforeTheDrain is a guard-coverage test.
//
// The shutdown path is: wait for either a signal or the producer's exit,
// then externalWait -> close(events) -> wait for sinkDone -> wait for the
// projector. Everything the producer already emitted is still sitting in
// the events channel buffer (256) when that select fires, and every one
// of those events has ALREADY been counted against the cursor. So a
// `return` inside the select discards them permanently: the next start
// resumes past them and the hole is silent.
//
// It is not a hypothetical path. pipeline.ProcessLedger recovers a
// decoder panic INTO a ledger error, and that error arrives on exactly
// this channel — so the failure mode that loses data is the same one a
// single malformed event triggers.
//
// This is asserted structurally rather than behaviourally because the
// sequence lives inline in realMain, which needs a full dependency tree
// to run. A structural guard that cannot be satisfied by accident is
// worth more here than a behavioural test nobody can build.
func ruleShutdownSelectNeverReturnsBeforeTheDrain(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	var checked int
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectStmt)
		if !ok {
			return true
		}
		// Identify the shutdown select by the channel it reads: the
		// producer's exit signal. Any other select is out of scope.
		src := nodeText(t, fset, sel)
		if !strings.Contains(src, "<-streamErr") || !strings.Contains(src, "rootCtx.Done()") {
			return true
		}
		checked++

		ast.Inspect(sel, func(inner ast.Node) bool {
			if ret, isRet := inner.(*ast.ReturnStmt); isRet {
				t.Errorf("the shutdown select at %s contains a `return` (%s). "+
					"Returning here skips externalWait, close(events) and the sinkDone wait, "+
					"discarding up to one channel buffer of ALREADY-CURSORED events (#368 M2). "+
					"Record the error in a variable and return it after the drain instead.",
					fset.Position(ret.Pos()), nodeText(t, fset, ret))
			}
			return true
		})
		return true
	})

	if checked != 1 {
		t.Fatalf("found %d shutdown select statement(s) reading streamErr + rootCtx.Done(), want exactly 1 — "+
			"if the shutdown path was restructured, re-point this guard at it rather than deleting it", checked)
	}
}

// TestIndexerActsOnSinkShutdownLoss pins the wiring: PersistEvents'
// ShutdownLoss must be kept and handed to the rewind once the sink is
// done. Discarding it leaves an abandoned trade behind a cursor that
// has already moved past it.
func ruleIndexerActsOnSinkShutdownLoss(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	isCall := func(e ast.Expr, name string) bool {
		call, ok := e.(*ast.CallExpr)
		if !ok {
			return false
		}
		switch fn := call.Fun.(type) {
		case *ast.SelectorExpr:
			return fn.Sel.Name == name
		case *ast.Ident:
			return fn.Name == name
		}
		return false
	}
	var assigned, discarded, rewoundOnSinkDone bool
	ast.Inspect(file, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.AssignStmt:
			if len(s.Rhs) == 1 && isCall(s.Rhs[0], "PersistEvents") {
				assigned = true
			}
		case *ast.ExprStmt:
			if isCall(s.X, "PersistEvents") {
				discarded = true
			}
		case *ast.CommClause:
			recv, ok := s.Comm.(*ast.ExprStmt)
			if !ok {
				return true
			}
			u, ok := recv.X.(*ast.UnaryExpr)
			if !ok || u.Op != token.ARROW {
				return true
			}
			if id, ok := u.X.(*ast.Ident); !ok || id.Name != "sinkDone" {
				return true
			}
			for _, st := range s.Body {
				if es, ok := st.(*ast.ExprStmt); ok && isCall(es.X, "rewindCursorForSinkLoss") {
					rewoundOnSinkDone = true
				}
			}
		}
		return true
	})
	if discarded || !assigned {
		t.Error("main.go discards pipeline.PersistEvents' ShutdownLoss — an abandoned on-chain trade is then only a counter and a log line")
	}
	if !rewoundOnSinkDone {
		t.Error("the `case <-sinkDone:` arm does not call rewindCursorForSinkLoss — the cursor stays past trades the sink abandoned")
	}
}
