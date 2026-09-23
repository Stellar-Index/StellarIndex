package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// TestCHLiveSinkDialIsNonFatalAndRetried is a structural guard for K024.
//
// clickhouse.NewLiveSink used to be dialled once, inline, in realMain: a
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
func TestCHLiveSinkDialIsNonFatalAndRetried(t *testing.T) {
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
