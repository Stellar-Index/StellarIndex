// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"bytes"
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	sep41supply "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_supply"
)

type fakeSEP41RollupResetter struct {
	calls     int
	contracts [][]string
	ctxErr    error
	err       error
}

func (f *fakeSEP41RollupResetter) ResetSEP41SupplyRollupFold(ctx context.Context, contractIDs []string) (int64, error) {
	f.calls++
	f.contracts = append(f.contracts, contractIDs)
	f.ctxErr = ctx.Err()
	return 3, f.err
}

func TestResetSEP41RollupAfterRebuild_FullResetOnSEP41SupplyWrite(t *testing.T) {
	f := &fakeSEP41RollupResetter{}
	var out bytes.Buffer
	if err := resetSEP41RollupAfterRebuild(context.Background(), &out, f, sep41supply.SourceName, true); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if f.calls != 1 {
		t.Fatalf("ResetSEP41SupplyRollupFold called %d times, want 1", f.calls)
	}
	if f.contracts[0] != nil {
		t.Errorf("contract scope = %v, want nil (FULL reset)", f.contracts[0])
	}
	if !strings.Contains(out.String(), "reset 3 sep41_supply_rollup fold row(s)") {
		t.Errorf("output %q does not report the reset row count", out.String())
	}
}

// An interrupted run's ctx is already cancelled, but the windows it wrote
// still sit below the checkpoint, so the reset must not inherit the cancel.
func TestResetSEP41RollupAfterRebuild_RunsOnCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := &fakeSEP41RollupResetter{}
	if err := resetSEP41RollupAfterRebuild(ctx, &bytes.Buffer{}, f, sep41supply.SourceName, true); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if f.calls != 1 || f.ctxErr != nil {
		t.Fatalf("calls=%d ctxErr=%v, want 1 call on a live context", f.calls, f.ctxErr)
	}
}

func TestResetSEP41RollupAfterRebuild_NoResetForDryRunOrOtherSources(t *testing.T) {
	for _, tc := range []struct {
		source string
		write  bool
	}{
		{sep41supply.SourceName, false},
		{"aquarius", true},
		{"sep41_transfers", true},
	} {
		f := &fakeSEP41RollupResetter{}
		var out bytes.Buffer
		if err := resetSEP41RollupAfterRebuild(context.Background(), &out, f, tc.source, tc.write); err != nil {
			t.Fatalf("%s write=%v: %v", tc.source, tc.write, err)
		}
		if f.calls != 0 {
			t.Errorf("%s write=%v: ResetSEP41SupplyRollupFold called %d times, want 0", tc.source, tc.write, f.calls)
		}
		if wantNote := tc.source == sep41supply.SourceName; strings.Contains(out.String(), "would then ResetSEP41SupplyRollupFold") != wantNote {
			t.Errorf("%s write=%v: dry-run note present=%v, want %v (output %q)", tc.source, tc.write, !wantNote, wantNote, out.String())
		}
	}
}

func TestResetSEP41RollupAfterRebuild_PropagatesError(t *testing.T) {
	boom := errors.New("boom")
	f := &fakeSEP41RollupResetter{err: boom}
	if err := resetSEP41RollupAfterRebuild(context.Background(), &bytes.Buffer{}, f, sep41supply.SourceName, true); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap the resetter's error", err)
	}
}

// TestProjectedRebuild_ResetsTheSEP41RollupAfterTheRun pins the wiring: the
// reset is only worth anything if projectedRebuild reaches it, and only
// AFTER RunProjectedRebuild — a reset before the writes lets the worker
// re-advance the checkpoint past rows the run has yet to write.
func TestProjectedRebuild_ResetsTheSEP41RollupAfterTheRun(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "projected_rebuild.go", nil, 0)
	if err != nil {
		t.Fatalf("parse projected_rebuild.go: %v", err)
	}
	var run, reset token.Pos
	for _, d := range file.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "projectedRebuild" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if id, ok := call.Fun.(*ast.Ident); ok {
				switch id.Name {
				case "RunProjectedRebuild":
					run = call.Pos()
				case "resetSEP41RollupAfterRebuild":
					reset = call.Pos()
				}
			}
			return true
		})
	}
	if run == token.NoPos {
		t.Fatal("projectedRebuild no longer calls RunProjectedRebuild — this guard has moved")
	}
	if reset == token.NoPos {
		t.Fatal("projectedRebuild never calls resetSEP41RollupAfterRebuild — a -write rebuild of sep41_supply " +
			"leaves its rows below the sep41_supply_rollup checkpoint, invisible to served supply")
	}
	if reset < run {
		t.Error("projectedRebuild resets the sep41 rollup BEFORE RunProjectedRebuild — the reset must follow the writes")
	}
}
