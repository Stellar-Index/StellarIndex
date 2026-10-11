package main

import (
	"go/ast"
	"go/token"
	"os"
	"regexp"
	"strings"
	"testing"
)

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
// Comments must be stripped: matching `externalWait()` inside explanatory prose above the code
// yields an empty span and a guard that passes while asserting nothing.
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
		// The drain is wrapped in waitBounded so a stuck connector cannot hang the exit. Match the
		// CALL SITE by the function it drains, not an exact line, so the guard keeps finding the
		// span; a missing span (end -1) must fail loudly.
		if start >= 0 && strings.Contains(line, "externalWait") && !strings.Contains(line, ":=") {
			end = i
			break
		}
	}
	if start < 0 || end < 0 {
		t.Fatalf("could not locate the shutdown select (%d) and externalWait() (%d) in main.go", start, end)
	}
	return code[start:end]
}

// TestNoReturnBetweenShutdownSelectAndDrain closes a hole in the guard above: moving
// `return fatalErr` to the line AFTER the select passes "no return INSIDE the select" yet still
// skips the drain.
//
// The real invariant is positional: between the shutdown select and externalWait(), any
// `return` skips the drain.
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

// TestShutdownCancelsRootBeforeDraining: the external connectors are bound to rootCtx, so
// draining without cancelling it first waits on goroutines nothing has asked to stop. The process
// would hang with its metrics server down and never exit, so systemd never restarts it.
func TestShutdownCancelsRootBeforeDraining(t *testing.T) {
	for _, line := range shutdownSpan(t) {
		if strings.TrimSpace(line) == "cancel()" {
			return
		}
	}
	t.Error("rootCtx is not cancelled between the shutdown select and externalWait() — " +
		"the external connectors are bound to rootCtx, so the WaitGroup can never drain and the process hangs")
}

// TestMetricsServerShutsDownAfterTheDrainNotBeforeIt: shutting the metrics server down right
// after cancel() would make /metrics unscrapable for the whole drain window, exactly when an
// operator needs to see drain progress.
//
// Asserted positionally, like the guards above: metricsSrv.Shutdown must appear AFTER the
// sinkDone wait, not between cancel() and it.
func TestMetricsServerShutsDownAfterTheDrainNotBeforeIt(t *testing.T) {
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

	sinkDoneIdx, metricsShutdownIdx := -1, -1
	for i, line := range code {
		if sinkDoneIdx < 0 && strings.Contains(line, "case <-sinkDone:") {
			sinkDoneIdx = i
		}
		if strings.Contains(line, "metricsSrv.Shutdown(") {
			metricsShutdownIdx = i
		}
	}
	if sinkDoneIdx < 0 {
		t.Fatalf("could not locate the sinkDone wait in main.go")
	}
	if metricsShutdownIdx < 0 {
		t.Fatalf("could not locate metricsSrv.Shutdown( in main.go")
	}
	if metricsShutdownIdx < sinkDoneIdx {
		t.Errorf("metricsSrv.Shutdown (line %d) runs BEFORE the sinkDone wait (line %d) — "+
			"the metrics endpoint goes down before the producer/connector/sink drain sequence runs, "+
			"making /metrics unscrapable for the whole drain window (Q106). Move the metrics shutdown "+
			"to after the drain completes.", metricsShutdownIdx+1, sinkDoneIdx+1)
	}
}
