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

	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
)

// indexBackfillSubcommands are the windowed INSERT…SELECT lake backfills.
// Each wrote on every run that named no flag at all (#868): there was no
// preview, so a mistyped -from started a multi-hour, billions-of-rows fill.
var indexBackfillSubcommands = []string{
	"ch-txindex-backfill",
	"ch-contract-ledgers-backfill",
	"ch-instance-backfill",
}

// TestIndexBackfills_RefuseWithoutAStatedMode: a run naming neither -write
// nor -dry-run is refused before any ClickHouse contact, so a scripted
// caller written for the old write-by-default contract fails loudly
// instead of previewing and exiting 0.
func TestIndexBackfills_RefuseWithoutAStatedMode(t *testing.T) {
	for _, verb := range indexBackfillSubcommands {
		err := Run([]string{verb, "-ch-addr", "127.0.0.1:1", "-from", "2", "-to", "3"})
		if !errors.Is(err, opsutil.ErrWriteModeUnstated) {
			t.Errorf("%s with no mode flag: err = %v, want opsutil.ErrWriteModeUnstated", verb, err)
		}
	}
}

// TestIndexBackfills_DryRunWritesNothing is the behavioural half: with the
// range resolvable (stubbed lake tip), -dry-run returns nil without the fill
// ever dialling ClickHouse, while -write reaches the fill and fails against
// an address nothing listens on.
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

// TestLakeBackfills_BuildTheirFlagSetWithTheWriteGate is the structural guard
// for the next windowed backfill: any chops file that calls a
// clickhouse.Backfill* writer must build its FlagSet through the shared
// fail-closed gate.
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
