// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// indexBackfillSubcommands are the windowed INSERT…SELECT lake backfills.
// Each wrote on every run that named no flag at all: there was no
// preview, so a mistyped -from started a multi-hour, billions-of-rows fill.
var indexBackfillSubcommands = []string{
	"ch-txindex-backfill",
	"ch-contract-ledgers-backfill",
	"ch-instance-backfill",
}

// TestIndexBackfills_RefuseWithoutAStatedMode: a run naming neither -write nor -dry-run is refused before any ClickHouse
// contact, so a scripted caller assuming write-by-default fails loudly instead of previewing and exiting 0.
func TestIndexBackfills_RefuseWithoutAStatedMode(t *testing.T) {
	for _, verb := range indexBackfillSubcommands {
		err := Run([]string{verb, "-ch-addr", "127.0.0.1:1", "-from", "2", "-to", "3"})
		if !errors.Is(err, opsutil.ErrWriteModeUnstated) {
			t.Errorf("%s with no mode flag: err = %v, want opsutil.ErrWriteModeUnstated", verb, err)
		}
	}
}

// TestIndexBackfills_DryRunWritesNothing is the behavioural half: with a resolvable range (stubbed lake tip), -dry-run
// returns nil without dialling ClickHouse, while -write reaches the fill and fails against an address nothing listens
// on.
func TestIndexBackfills_DryRunWritesNothing(t *testing.T) {
	orig := contiguousLakeTip
	contiguousLakeTip = func(context.Context, string, uint32) (uint32, error) { return 1_000, nil }
	t.Cleanup(func() { contiguousLakeTip = orig })

	for _, verb := range indexBackfillSubcommands {
		base := []string{verb, "-ch-addr", "127.0.0.1:1", "-from", "2", "-to", "3"}
		if err := Run(append(append([]string{}, base...), "-dry-run")); err != nil {
			t.Errorf("%s -dry-run: err = %v, want nil — a preview must not reach the fill", verb, err)
		}
		err := Run(append(append([]string{}, base...), "-write"))
		if err == nil || errors.Is(err, opsutil.ErrWriteModeUnstated) {
			t.Errorf("%s -write against an unreachable lake: err = %v, want the fill's ClickHouse error", verb, err)
		}
	}
}

// TestLakeBackfills_BuildTheirFlagSetWithTheWriteGate is the structural guard for the next windowed backfill:
// any chops file calling a clickhouse.Backfill* writer must build its FlagSet through the shared fail-closed gate.
func TestLakeBackfills_BuildTheirFlagSetWithTheWriteGate(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	writers := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, rerr := fs.ReadFile(os.DirFS("."), f)
		if rerr != nil {
			t.Fatal(rerr)
		}
		s := string(src)
		if !strings.Contains(s, "clickhouse.Backfill") {
			continue
		}
		writers++
		if !strings.Contains(s, "opsutil.NewMutatingFlagSet(") && !strings.Contains(s, "opsutil.RegisterWriteGate(") {
			t.Errorf("%s calls a clickhouse.Backfill* writer but builds no shared write gate: "+
				"use opsutil.NewMutatingFlagSet so it previews unless -write is passed (#868)", f)
		}
	}
	if writers < len(indexBackfillSubcommands) {
		t.Fatalf("found %d clickhouse.Backfill* callers, want at least %d — the scan is not seeing the package", writers, len(indexBackfillSubcommands))
	}
}

// caseParseTxIndexBackfillFlags_RefusesBareFullHistory pins the safe default: a BARE
// `ch-txindex-backfill` (no bounds, no -full) must NOT resolve into the implicit ledger-2..tip
// backfill, a heavy job that must be babysat. The full run needs an explicit -full.
func caseParseTxIndexBackfillFlags_RefusesBareFullHistory(t *testing.T) {
	_, err := parseTxIndexBackfillFlags([]string{"-write"})
	if err == nil {
		t.Fatal("bare invocation (no -from/-to/-full) was accepted — it must refuse the " +
			"implicit full-history backfill")
	}
	if !strings.Contains(err.Error(), "full-history") {
		t.Fatalf("refusal error should explain the full-history footgun, got: %v", err)
	}
}

// caseParseTxIndexBackfillFlags_ExplicitOptInsAreAccepted pins that each
// intentional path still works and resolves to the expected plan.
func caseParseTxIndexBackfillFlags_ExplicitOptInsAreAccepted(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantFrom uint32
		wantTo   uint32
	}{
		{"full opts into 2..tip", []string{"-full", "-write"}, 2, 0},
		{"explicit from is a resume point", []string{"-from", "80000000", "-write"}, 80_000_000, 0},
		{"explicit to bounds the range", []string{"-to", "1000000", "-dry-run"}, 2, 1_000_000},
		{"explicit from+to", []string{"-from", "500000", "-to", "1000000", "-write"}, 500_000, 1_000_000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := parseTxIndexBackfillFlags(tc.args)
			if err != nil {
				t.Fatalf("parse(%v) errored: %v", tc.args, err)
			}
			if plan.from != tc.wantFrom {
				t.Errorf("from = %d, want %d", plan.from, tc.wantFrom)
			}
			if plan.to != tc.wantTo {
				t.Errorf("to = %d, want %d", plan.to, tc.wantTo)
			}
		})
	}
}

// caseParseTxIndexBackfillFlags_ZeroFromStillRejected keeps the pre-existing
// invariant: -from 0 and -window 0 are invalid regardless of the new guard.
func caseParseTxIndexBackfillFlags_ZeroFromStillRejected(t *testing.T) {
	if _, err := parseTxIndexBackfillFlags([]string{"-from", "0", "-write"}); err == nil {
		t.Error("-from 0 must be rejected")
	}
	if _, err := parseTxIndexBackfillFlags([]string{"-full", "-window", "0", "-write"}); err == nil {
		t.Error("-window 0 must be rejected")
	}
}

// TestParseTxIndexBackfillFlags groups the ch-txindex-backfill flag contracts as named subtests.
func TestParseTxIndexBackfillFlags(t *testing.T) {
	t.Run("ParseTxIndexBackfillFlags_RefusesBareFullHistory", caseParseTxIndexBackfillFlags_RefusesBareFullHistory)
	t.Run("ParseTxIndexBackfillFlags_ExplicitOptInsAreAccepted", caseParseTxIndexBackfillFlags_ExplicitOptInsAreAccepted)
	t.Run("ParseTxIndexBackfillFlags_ZeroFromStillRejected", caseParseTxIndexBackfillFlags_ZeroFromStillRejected)
}

func TestTxIndexBackfillCoversHistory(t *testing.T) {
	cases := []struct {
		name string
		plan txIndexBackfillPlan
		want bool
	}{
		{"genesis to tip", txIndexBackfillPlan{from: 2, to: 0}, true},
		{"resume run", txIndexBackfillPlan{from: 5_000_000, to: 0}, false},
		{"explicit upper bound", txIndexBackfillPlan{from: 2, to: 1_000}, false},
	}
	for _, tc := range cases {
		if got := tc.plan.coversHistory(); got != tc.want {
			t.Errorf("%s: coversHistory = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestRunInstanceBackfill_GenesisWatermark(t *testing.T) {
	origB, origS, origR, origT := backfillInstanceChanges, setInstanceGenesisMark, readInstanceStart, resolveInstanceTop
	t.Cleanup(func() {
		backfillInstanceChanges, setInstanceGenesisMark, readInstanceStart, resolveInstanceTop = origB, origS, origR, origT
	})
	logf := func(string, ...any) {}

	created := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	good := clickhouse.InstanceBackfillStart{
		RawMaxLedger: 500, MVExists: true, MVUUID: "u1", MVModified: created, MVAge: time.Hour,
	}
	with := func(f func(*clickhouse.InstanceBackfillStart)) clickhouse.InstanceBackfillStart {
		st := good
		f(&st)
		return st
	}
	recreated := with(func(s *clickhouse.InstanceBackfillStart) {
		s.MVUUID, s.MVModified, s.MVAge = "u2", created.Add(2*time.Hour), time.Second
	})
	dropped := clickhouse.InstanceBackfillStart{RawMaxLedger: 500}
	tests := []struct {
		name     string
		from, to uint32
		tip      uint32 // contiguous tip resolved after the start read; 0 = 500
		start    clickhouse.InstanceBackfillStart
		end      *clickhouse.InstanceBackfillStart // state after the run; nil = start
		startErr error
		backfill error
		wantMark bool
		wantErr  bool
	}{
		{name: "complete genesis run", from: 2, start: good, wantMark: true},
		{name: "ledgers closed between the start read and the tip", from: 2, tip: 510, start: good, wantMark: true},
		{name: "partial run above genesis", from: 1000, tip: 1500, start: good},
		{name: "explicit -to", from: 2, to: 500, start: good},
		{name: "hole below the raw tip", from: 2, start: with(func(s *clickhouse.InstanceBackfillStart) { s.RawMaxLedger = 900 })},
		{name: "no materialized view", from: 2, start: clickhouse.InstanceBackfillStart{RawMaxLedger: 500}},
		{name: "view younger than the settle margin", from: 2, start: with(func(s *clickhouse.InstanceBackfillStart) { s.MVAge = time.Minute })},
		{name: "view dropped during the run", from: 2, start: good, end: &dropped},
		{name: "view recreated during the run", from: 2, start: good, end: &recreated},
		{name: "start state unreadable", from: 2, startErr: errors.New("down")},
		{name: "failed run", from: 2, start: good, backfill: errors.New("boom"), wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotThru uint32
			marked := false
			reads := 0
			readInstanceStart = func(context.Context, string, string) (clickhouse.InstanceBackfillStart, error) {
				reads++
				if reads > 1 && tc.end != nil {
					return *tc.end, nil
				}
				return tc.start, tc.startErr
			}
			tip := tc.tip
			if tip == 0 {
				tip = 500
			}
			resolveInstanceTop = func(context.Context, string, uint32, uint32) (uint32, error) {
				if reads != 1 {
					t.Fatalf("tip resolved after %d start reads, want 1: the raw tip must be read first", reads)
				}
				return tip, nil
			}
			backfillInstanceChanges = func(context.Context, string, string, uint32, uint32, uint32, func(string, ...any)) error {
				return tc.backfill
			}
			setInstanceGenesisMark = func(_ context.Context, _, _ string, thru uint32) error {
				marked, gotThru = true, thru
				return nil
			}
			confirm := func(uint32) bool { return true }
			err := runInstanceBackfill(context.Background(), "addr", "contract_instance_changes", tc.from, tc.to, 100, confirm, logf)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if marked != tc.wantMark || (marked && gotThru != tip) {
				t.Fatalf("marked=%v thru=%d, want marked=%v thru=%d", marked, gotThru, tc.wantMark, tip)
			}
		})
	}
}
