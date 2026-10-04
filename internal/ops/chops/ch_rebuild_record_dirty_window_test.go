// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

// `ch-rebuild -record-dirty-window` is the record an emptied clean-slate
// window owes the ADR-0033 completeness verdict (F075). What it writes has
// to be exactly right to be worth anything: one row PER SOURCE (the table
// is keyed by source and the verdict looks its window up by the source it
// is verifying), under a catalogue name, over the emptied range, with a
// reason that says which tool emptied it.

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

type recordedWindow = timescale.ProjectionDirtyWindow

// fakeDirtyWindowRecorder stands in for the Postgres store at the one call
// [recordCHRebuildDirtyWindows] makes. The SQL itself is
// timescale.Store.RecordProjectionDirtyWindow's, unchanged and already
// covered; what is new is WHAT this path asks it to write.
type fakeDirtyWindowRecorder struct {
	got    []recordedWindow
	failOn string
}

func (f *fakeDirtyWindowRecorder) RecordProjectionDirtyWindow(_ context.Context, w recordedWindow) error {
	if w.Source == f.failOn {
		return errors.New("connection refused")
	}
	f.got = append(f.got, w)
	return nil
}

func TestRecordCHRebuildDirtyWindows_OneWindowPerDeletedSource(t *testing.T) {
	t.Parallel()
	rec := &fakeDirtyWindowRecorder{}
	var out strings.Builder
	if err := recordCHRebuildDirtyWindows(context.Background(), rec, &out, 61_000_000, 61_999_999,
		[]string{"soroswap", "cctp"}, timescale.CHRebuildEmptiedReason); err != nil {
		t.Fatalf("recordCHRebuildDirtyWindows: %v", err)
	}
	if len(rec.got) != 2 {
		t.Fatalf("recorded %d window(s), want one per source: %+v", len(rec.got), rec.got)
	}
	for i, want := range []string{"soroswap", "cctp"} {
		got := rec.got[i]
		if got.Source != want {
			t.Errorf("window %d source = %q, want %q", i, got.Source, want)
		}
		if got.From != 61_000_000 || got.To != 61_999_999 {
			t.Errorf("window %d range = [%d,%d], want [61000000,61999999] — the emptied window, not the whole run",
				i, got.From, got.To)
		}
		if want := "ch-rebuild-projected emptied [61000000,61999999]"; got.Reason != want {
			t.Errorf("window %d reason = %q, want %q", i, got.Reason, want)
		}
		if got.IsProjectorReplay() {
			t.Errorf("window %d classifies as a projector-replay rewind — that would SUPPRESS the projector lag alert for a source whose rows are missing", i)
		}
	}
	if line := out.String(); !strings.Contains(line, "[61000000,61999999]") || !strings.Contains(line, "soroswap,cctp") {
		t.Errorf("report line = %q, want the range and the sources", line)
	}
}

// A store that refuses must not be reported as a filed obligation: the
// caller is deciding whether a hole is still uncertified.
func TestRecordCHRebuildDirtyWindows_StoreFailureIsNotSwallowed(t *testing.T) {
	t.Parallel()
	rec := &fakeDirtyWindowRecorder{failOn: "cctp"}
	var out strings.Builder
	err := recordCHRebuildDirtyWindows(context.Background(), rec, &out, 61_000_000, 61_999_999,
		[]string{"soroswap", "cctp"}, timescale.CHRebuildEmptiedReason)
	if err == nil {
		t.Fatal("a store that refused the write reported success")
	}
	if !strings.Contains(err.Error(), "cctp") {
		t.Errorf("error %q does not name the source whose window was NOT filed", err)
	}
	if out.String() != "" {
		t.Errorf("printed %q although the obligation was not filed", out.String())
	}
}

// Recording nothing is not a quiet success — the caller asked for an
// obligation and would otherwise believe it exists.
func TestRecordCHRebuildDirtyWindows_EmptySourceSetIsAnError(t *testing.T) {
	t.Parallel()
	rec := &fakeDirtyWindowRecorder{}
	var out strings.Builder
	if err := recordCHRebuildDirtyWindows(context.Background(), rec, &out, 1, 2, nil, timescale.CHRebuildEmptiedReason); err == nil {
		t.Fatal("recording zero windows reported success")
	}
	if len(rec.got) != 0 {
		t.Errorf("wrote %+v", rec.got)
	}
}

func TestChRebuildRecordDirtySources(t *testing.T) {
	t.Parallel()
	cat := []reconSource{{name: "soroswap"}, {name: "cctp"}, {name: "blend"}}
	t.Run("named catalogue sources pass through in order", func(t *testing.T) {
		t.Parallel()
		got, err := chRebuildRecordDirtySources(cat, []string{"cctp", "soroswap"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if strings.Join(got, ",") != "cctp,soroswap" {
			t.Errorf("got %v, want the named sources verbatim", got)
		}
	})
	t.Run("a name no catalogue entry carries is refused", func(t *testing.T) {
		t.Parallel()
		// A window filed under a name no verdict reads is a silent
		// no-op dressed as an obligation — worse than no record, because
		// the operator believes the hole is covered.
		_, err := chRebuildRecordDirtySources(cat, []string{"cctp", "sorosw4p"})
		if err == nil {
			t.Fatal("a non-catalogue source was accepted")
		}
		if !strings.Contains(err.Error(), "sorosw4p") {
			t.Errorf("error %q does not name the offender", err)
		}
	})
	t.Run("no -sources is refused rather than filing the whole catalogue", func(t *testing.T) {
		t.Parallel()
		if _, err := chRebuildRecordDirtySources(cat, nil); err == nil {
			t.Fatal("an empty source list was accepted: every catalogue source would owe a full re-reconcile")
		}
	})
}

func TestCheckCHRebuildRecordDirtyFlags(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name                          string
		recordDirty, preflight, write bool
		wantErr                       bool
	}{
		{name: "the documented invocation", recordDirty: true},
		{name: "with -write", recordDirty: true, write: true, wantErr: true},
		{name: "with -preflight", recordDirty: true, preflight: true, wantErr: true},
		{name: "an ordinary -write run is untouched", write: true},
		{name: "a preflight is untouched", preflight: true, write: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := checkCHRebuildRecordDirtyFlags(tc.recordDirty, tc.preflight, tc.write)
			if (err != nil) != tc.wantErr {
				t.Errorf("checkCHRebuildRecordDirtyFlags(%v,%v,%v) = %v, wantErr %v",
					tc.recordDirty, tc.preflight, tc.write, err, tc.wantErr)
			}
		})
	}
}

// A -write run rewrites served rows below the completeness watermark. Before
// it writes, it must file the same obligation projector-replay and
// projected-rebuild file, one per re-derived source, or the next verdict
// carries its prior clean claim over the rewritten range.
func TestCHRebuild_WriteRecordsDirtyWindowBeforeWriting(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile("ch_rebuild.go")
	if err != nil {
		t.Fatalf("read ch_rebuild.go: %v", err)
	}
	body := funcBodyFrom(string(b), "chRebuild")
	if body == "" {
		t.Fatal("chRebuild not found — this test is asserting nothing")
	}
	record := strings.Index(body, "recordCHRebuildDirtyWindows(ctx, store, os.Stderr, lo, hi, rederived, timescale.CHRebuildWriteReason)")
	preflight := strings.Index(body, "return reportCHRebuildPreflight(")
	writer := strings.Index(body, "drainAndWrite(")
	if preflight < 0 || writer < 0 {
		t.Fatalf("anchors not found (preflight %d, writer %d) — this test is asserting nothing", preflight, writer)
	}
	switch {
	case record < 0:
		t.Fatal("chRebuild -write records no projection dirty window: the next completeness verdict carries its prior clean claim over the rewritten range")
	case record < preflight:
		t.Error("the record precedes the -preflight stop: a preflight, which writes nothing, would file an obligation")
	case record > writer:
		t.Error("the record follows the writer: a run that fails partway leaves rewritten rows certified by stale evidence")
	}
	if !strings.Contains(body[record-300:record], "if write {") {
		t.Error("the record is not gated on -write: a dry run would file an obligation")
	}
	if !strings.Contains(body[record:record+300], "return ") {
		t.Error("a failed record does not stop the run: it would write without the obligation filed")
	}
}

func TestRecordCHRebuildDirtyWindows_WriteReason(t *testing.T) {
	t.Parallel()
	rec := &fakeDirtyWindowRecorder{}
	var out strings.Builder
	if err := recordCHRebuildDirtyWindows(context.Background(), rec, &out, 62_000_000, 62_999_999,
		[]string{"sdex"}, timescale.CHRebuildWriteReason); err != nil {
		t.Fatalf("recordCHRebuildDirtyWindows: %v", err)
	}
	if len(rec.got) != 1 {
		t.Fatalf("recorded %+v, want one window", rec.got)
	}
	got := rec.got[0]
	if want := "ch-rebuild -write [62000000,62999999]"; got.Reason != want {
		t.Errorf("reason = %q, want %q", got.Reason, want)
	}
	if got.IsProjectorReplay() {
		t.Error("a ch-rebuild -write window classifies as a projector-replay rewind — it would suppress the projector lag alert")
	}
}
