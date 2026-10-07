package chops

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/sources/band"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

type fakeOracleSweeper struct {
	calls []timescale.OracleRederiveSweep
}

func (f *fakeOracleSweeper) SweepOracleRederive(_ context.Context, sw timescale.OracleRederiveSweep) (timescale.OracleRederiveSweepResult, error) {
	f.calls = append(f.calls, sw)
	return timescale.OracleRederiveSweepResult{Deleted: 3}, nil
}

func TestOracleRederiveSweepScope(t *testing.T) {
	for _, tc := range []struct {
		source             string
		sweep, assetScoped bool
	}{
		{"reflector-dex", true, false},
		{"reflector-cex", true, false},
		{"reflector-fx", true, false},
		{"redstone", true, false},
		{"band", true, true}, // nested relays share op_index
		{"soroswap-router", false, false},
		{"blend_backstop", false, false},
	} {
		sweep, scoped := oracleRederiveSweepScope(tc.source)
		if sweep != tc.sweep || scoped != tc.assetScoped {
			t.Errorf("%s: sweep=%v assetScoped=%v, want %v %v", tc.source, sweep, scoped, tc.sweep, tc.assetScoped)
		}
	}
}

func TestSweepOracleRederiveGeneration(t *testing.T) {
	var f fakeOracleSweeper
	var out bytes.Buffer
	if err := sweepOracleRederive(context.Background(), &out, &f, "ch-rebuild", "band", 10, 20, 77, true); err != nil {
		t.Fatal(err)
	}
	if err := sweepOracleRederive(context.Background(), &out, &f, "ch-rebuild", "band", 10, 20, 77, false); err != nil {
		t.Fatal(err)
	}
	want := []timescale.OracleRederiveSweep{
		{Source: "band", From: 10, To: 20, Generation: 77, AssetScoped: true},
		{Source: "band", From: 10, To: 20, AssetScoped: true, DryRun: true},
	}
	if len(f.calls) != 2 || f.calls[0] != want[0] || f.calls[1] != want[1] {
		t.Fatalf("calls = %+v, want %+v", f.calls, want)
	}
	if !bytes.Contains(out.Bytes(), []byte("would delete 3")) {
		t.Errorf("dry run must print the would-delete count:\n%s", out.String())
	}
}

func TestSweepProjectedRebuildOracleOnlyAfterCompleteRun(t *testing.T) {
	for name, tc := range map[string]struct {
		r           ProjectedRebuildResult
		runErr      error
		interrupted bool
		wantCall    bool
	}{
		"complete":    {wantCall: true},
		"interrupted": {interrupted: true},
		"failed":      {runErr: errors.New("boom")},
		"held window": {r: ProjectedRebuildResult{WindowsHeld: 1}},
	} {
		var f fakeOracleSweeper
		if err := sweepProjectedRebuildOracle(context.Background(), &bytes.Buffer{}, &f, "redstone", 1, 2, 9, true, tc.r, tc.runErr, tc.interrupted); err != nil {
			t.Fatal(err)
		}
		if got := len(f.calls) == 1; got != tc.wantCall {
			t.Errorf("%s: swept=%v, want %v", name, got, tc.wantCall)
		}
	}
}

func TestSweepCHRebuildOracleSkipsFailedAndNonContractCallRuns(t *testing.T) {
	cat := []reconSource{{name: "band", callDec: band.NewDecoder("CBAND")}}
	all := func(string) bool { return true }
	var f fakeOracleSweeper
	if err := sweepCHRebuildOracle(context.Background(), &bytes.Buffer{}, &f, cat, all, false, nil, 1, 2, 9, true); err != nil || len(f.calls) != 0 {
		t.Fatalf("without -contract-calls: err=%v calls=%d, want none", err, len(f.calls))
	}
	if err := sweepCHRebuildOracle(context.Background(), &bytes.Buffer{}, &f, cat, all, true, map[string]int{"band": 1}, 1, 2, 9, true); err != nil || len(f.calls) != 0 {
		t.Fatalf("with a failed band write: err=%v calls=%d, want none", err, len(f.calls))
	}
	if err := sweepCHRebuildOracle(context.Background(), &bytes.Buffer{}, &f, cat, all, true, nil, 1, 2, 9, true); err != nil || len(f.calls) != 1 || f.calls[0].Generation != 9 {
		t.Fatalf("complete band run: err=%v calls=%+v, want one at generation 9", err, f.calls)
	}
}
