package chops

import (
	"context"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// fakeProofLake stubs the lake reads for a stellar.ledgers holding [lo, hi] minus holes, the way ContiguousWatermark
// and MaxLedger answer it.
func fakeProofLake(t *testing.T, lo, hi uint32, holes ...uint32) {
	t.Helper()
	missing := map[uint32]bool{}
	for _, h := range holes {
		missing[h] = true
	}
	has := func(l uint32) bool { return l >= lo && l <= hi && !missing[l] }
	origTip, origMax := contiguousLakeTip, lakeMaxLedger
	t.Cleanup(func() { contiguousLakeTip, lakeMaxLedger = origTip, origMax })
	contiguousLakeTip = func(_ context.Context, _ string, from uint32) (uint32, error) {
		l := from
		for has(l) {
			l++
		}
		return l - 1, nil
	}
	lakeMaxLedger = func(context.Context, string) (uint32, error) { return hi, nil }
}

// TestChIndexProof_GenesisIsTheLakesFirstLedger drives the command through its flags and the real contiguity check:
// exported lakes start at ledger 2, so the default run must prove from 2 and record, while a hole anywhere refuses.
func TestChIndexProof_GenesisIsTheLakesFirstLedger(t *testing.T) {
	origP, origM, origS := proveOpsWindow, opsMVsExist, setOpsProofMark
	t.Cleanup(func() { proveOpsWindow, opsMVsExist, setOpsProofMark = origP, origM, origS })
	opsMVsExist = func(context.Context, string) (bool, error) { return true, nil }

	cases := []struct {
		name      string
		args      []string
		lo, hi    uint32
		holes     []uint32
		wantMark  uint32
		wantErr   string
		wantFirst uint32 // first proved window's start; 0 = no window proved
	}{
		{name: "default run on a lake from 2", args: []string{"-write"}, lo: 2, hi: 2_500, wantMark: 2_500, wantFirst: 2},
		{name: "-from 1 is genesis", args: []string{"-from", "1", "-write"}, lo: 2, hi: 2_500, wantMark: 2_500, wantFirst: 2},
		{name: "ledger 2 missing", args: []string{"-write"}, lo: 3, hi: 2_500, wantErr: "missing ledger 2"},
		{name: "mid-range hole", args: []string{"-write"}, lo: 2, hi: 2_500, holes: []uint32{1_500}, wantErr: "missing ledger 1500"},
		{name: "later -from checks only", args: []string{"-from", "1002", "-write"}, lo: 2, hi: 2_500, wantFirst: 1_002},
		{name: "dry run", args: []string{"-dry-run"}, lo: 2, hi: 2_500, wantFirst: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fakeProofLake(t, tc.lo, tc.hi, tc.holes...)
			var marked, first uint32
			proveOpsWindow = func(_ context.Context, _ string, lo, hi uint32) (clickhouse.OpsBySourceWindow, error) {
				if first == 0 {
					first = lo
				}
				return clickhouse.OpsBySourceWindow{From: lo, To: hi, Txs: 1, ProjTxs: 1, Ops: 1, ProjOps: 1}, nil
			}
			setOpsProofMark = func(_ context.Context, _ string, thru uint32) error { marked = thru; return nil }

			err := chIndexProof(append([]string{"-ch-addr", "x", "-window", "1000"}, tc.args...))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
			} else if err != nil {
				t.Fatalf("err = %v", err)
			}
			if marked != tc.wantMark {
				t.Errorf("recorded watermark %d, want %d", marked, tc.wantMark)
			}
			if first != tc.wantFirst {
				t.Errorf("first proved window starts at %d, want %d", first, tc.wantFirst)
			}
		})
	}
}

// TestRunOpsBySourceProof_RecordsOnlyAProvenGenesisRun: the watermark tells the account pages ops_by_source is
// complete, so any run that did not check every window from genesis to the tip with both views live records nothing.
func TestRunOpsBySourceProof_RecordsOnlyAProvenGenesisRun(t *testing.T) {
	origP, origM, origS, origT, origX := proveOpsWindow, opsMVsExist, setOpsProofMark, resolveOpsProofTop, lakeMaxLedger
	t.Cleanup(func() {
		proveOpsWindow, opsMVsExist, setOpsProofMark, resolveOpsProofTop, lakeMaxLedger = origP, origM, origS, origT, origX
	})
	lakeMaxLedger = func(context.Context, string) (uint32, error) { return 2_500, nil }
	resolveOpsProofTop = func(_ context.Context, _ string, _, to uint32) (uint32, error) {
		if to != 0 {
			return to, nil
		}
		return 2_500, nil
	}

	cases := []struct {
		name       string
		from, to   uint32
		write      bool
		mvs        []bool // before, after
		missAt     uint32 // window start whose projection is one op short; 0 = none
		wantMark   uint32
		wantErr    string
		wantWindow [][2]uint32
	}{
		{
			name: "genesis to tip", from: 2, write: true, mvs: []bool{true, true}, wantMark: 2_500,
			wantWindow: [][2]uint32{{2, 1001}, {1002, 2001}, {2002, 2500}},
		},
		{name: "dry run", from: 2, mvs: []bool{true, true}},
		{name: "resumed run", from: 1002, write: true, mvs: []bool{true, true}},
		{name: "explicit to", from: 2, to: 2_000, write: true, mvs: []bool{true, true}},
		{name: "view born mid-run", from: 2, write: true, mvs: []bool{false, true}},
		{name: "view dropped mid-run", from: 2, write: true, mvs: []bool{true, false}},
		{
			name: "mismatch stops", from: 2, write: true, mvs: []bool{true, true}, missAt: 1002,
			wantErr: "misses keys in [1002,2001]", wantWindow: [][2]uint32{{2, 1001}, {1002, 2001}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var marked uint32
			var windows [][2]uint32
			mvCall := 0
			opsMVsExist = func(context.Context, string) (bool, error) {
				v := tc.mvs[mvCall]
				mvCall++
				return v, nil
			}
			proveOpsWindow = func(_ context.Context, _ string, lo, hi uint32) (clickhouse.OpsBySourceWindow, error) {
				windows = append(windows, [2]uint32{lo, hi})
				w := clickhouse.OpsBySourceWindow{From: lo, To: hi, Txs: 10, ProjTxs: 10, Ops: 30, ProjOps: 30}
				if lo == tc.missAt {
					w.ProjOps--
				}
				return w, nil
			}
			setOpsProofMark = func(_ context.Context, _ string, thru uint32) error { marked = thru; return nil }

			err := runOpsBySourceProof(context.Background(), "x", tc.from, tc.to, 1_000, tc.write, func(string, ...any) {})
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
			} else if err != nil {
				t.Fatalf("err = %v", err)
			}
			if marked != tc.wantMark {
				t.Errorf("recorded watermark %d, want %d", marked, tc.wantMark)
			}
			if tc.wantWindow != nil && len(windows) != len(tc.wantWindow) {
				t.Fatalf("windows %v, want %v", windows, tc.wantWindow)
			}
			for i, w := range tc.wantWindow {
				if windows[i] != w {
					t.Errorf("window %d = %v, want %v", i, windows[i], w)
				}
			}
		})
	}
}
