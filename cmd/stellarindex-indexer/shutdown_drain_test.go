package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// TestShutdownSelectNeverReturnsBeforeTheDrain is a guard-coverage test
// for #368 M2.
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
func TestShutdownSelectNeverReturnsBeforeTheDrain(t *testing.T) {
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

func nodeText(t *testing.T, fset *token.FileSet, n ast.Node) string {
	t.Helper()
	data, err := readMainGo()
	if err != nil {
		return ""
	}
	start, end := fset.Position(n.Pos()).Offset, fset.Position(n.End()).Offset
	if start < 0 || end > len(data) || start >= end {
		return ""
	}
	return string(data[start:end])
}

func readMainGo() ([]byte, error) { return os.ReadFile("main.go") }
