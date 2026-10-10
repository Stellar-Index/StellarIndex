// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"bytes"
	"context"
	"errors"
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
