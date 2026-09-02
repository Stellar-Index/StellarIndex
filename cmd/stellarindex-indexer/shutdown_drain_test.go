package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
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

// shutdownSpan returns the CODE lines between the shutdown select's
// producer arm and the externalWait() call, with comments stripped.
//
// Comment-stripping is not tidiness: the first attempt at these guards
// matched `externalWait()` inside the explanatory comment ABOVE the code,
// so the span it measured was empty and the guard passed while asserting
// nothing. A structural test that can be satisfied by prose is worse than
// no test.
func shutdownSpan(t *testing.T) []string {
	t.Helper()
	data, err := readMainGo()
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	var code []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		code = append(code, line)
	}
	start, end := -1, -1
	for i, line := range code {
		if start < 0 && strings.Contains(line, "case err := <-streamErr:") {
			start = i
		}
		if start >= 0 && strings.TrimSpace(line) == "externalWait()" {
			end = i
			break
		}
	}
	if start < 0 || end < 0 {
		t.Fatalf("could not locate the shutdown select (%d) and externalWait() (%d) in main.go", start, end)
	}
	return code[start:end]
}

// TestNoReturnBetweenShutdownSelectAndDrain closes the hole an independent
// review found in the guard above: moving `return fatalErr` to the line
// AFTER the select satisfies "no return INSIDE the select" while
// reintroducing the exact bug — the drain is skipped just the same.
//
// The real invariant is positional: between the shutdown select and
// externalWait(), any `return` skips the drain.
func TestNoReturnBetweenShutdownSelectAndDrain(t *testing.T) {
	// Match the `return` TOKEN anywhere on the line, not just at the start.
	// A prefix check misses `if fatalErr != nil { return fatalErr }`, which
	// is the most natural way someone would reintroduce this bug — verified
	// by mutation: the prefix version passed on exactly that line.
	returnTok := regexp.MustCompile(`\breturn\b`)
	for i, line := range shutdownSpan(t) {
		trimmed := strings.TrimSpace(line)
		if returnTok.MatchString(trimmed) {
			t.Errorf("line %d of the span between the shutdown select and externalWait() is a `return` (%q). "+
				"Returning anywhere in this span skips the drain and discards up to one channel buffer of "+
				"already-cursored events (#368 M2). Record the error and return it AFTER the drain.", i+1, trimmed)
		}
	}
}

// TestShutdownCancelsRootBeforeDraining pins the fix for the hang that
// d160215b introduced. The external connectors are bound to rootCtx, so
// draining without cancelling it first waits on goroutines nothing has
// asked to stop: the process hangs with its metrics server already down,
// never exits, and systemd never restarts it. r1 runs seven external
// connectors, so one MinIO blip would have wedged ingest silently.
func TestShutdownCancelsRootBeforeDraining(t *testing.T) {
	for _, line := range shutdownSpan(t) {
		if strings.TrimSpace(line) == "cancel()" {
			return
		}
	}
	t.Error("rootCtx is not cancelled between the shutdown select and externalWait() — " +
		"the external connectors are bound to rootCtx, so the WaitGroup can never drain and the process hangs")
}
