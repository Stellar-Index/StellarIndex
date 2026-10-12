package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/archivecompleteness"
	"github.com/Stellar-Index/StellarIndex/internal/hashdb"
	"github.com/Stellar-Index/StellarIndex/internal/ledgerstream"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// TestClassifyHashDBVerifySweep_DriftSurvivesShutdownCancel pins that drift tallied before a
// shutdown cancel is still classified as drift. The Canceled early return must not run ahead
// of the AnyDrift() check, or the drift is discarded.
func TestClassifyHashDBVerifySweep_DriftSurvivesShutdownCancel(t *testing.T) {
	res := archivecompleteness.HashDBVerifyResult{
		From: 100, To: 104,
		Verified: 3, Drifted: 1, Missing: 0, OutOfRange: 0,
		DriftSeqs: []uint32{102},
	}
	got := classifyHashDBVerifySweep(res, context.Canceled, 100, 104)
	if got != sweepOutcomeDrift {
		t.Fatalf("classifyHashDBVerifySweep() = %v, want sweepOutcomeDrift — drift tallied before a shutdown cancel must not be discarded", got)
	}
}

// TestClassifyHashDBVerifySweep_ShutdownWithoutDriftIsSilent pins the
// companion case: a cancel with no drift found stays "nothing to
// report", not "ok" and not "error" — a partial sweep is neither.
func TestClassifyHashDBVerifySweep_ShutdownWithoutDriftIsSilent(t *testing.T) {
	res := archivecompleteness.HashDBVerifyResult{
		From: 100, To: 104,
		Verified: 2, Drifted: 0, Missing: 0, OutOfRange: 0,
	}
	got := classifyHashDBVerifySweep(res, context.Canceled, 100, 104)
	if got != sweepOutcomeShutdown {
		t.Fatalf("classifyHashDBVerifySweep() = %v, want sweepOutcomeShutdown", got)
	}
}

// TestClassifyHashDBVerifySweep_DriftOutranksOtherStreamError confirms
// drift also wins over a non-Canceled stream error, matching the
// pre-existing "drift first" switch ordering.
func TestClassifyHashDBVerifySweep_DriftOutranksOtherStreamError(t *testing.T) {
	res := archivecompleteness.HashDBVerifyResult{
		From: 100, To: 104,
		Verified: 3, Drifted: 1,
	}
	got := classifyHashDBVerifySweep(res, errors.New("object not found"), 100, 104)
	if got != sweepOutcomeDrift {
		t.Fatalf("classifyHashDBVerifySweep() = %v, want sweepOutcomeDrift", got)
	}
}

// TestClassifyHashDBVerifySweep_OKWhenComplete pins the clean case.
func TestClassifyHashDBVerifySweep_OKWhenComplete(t *testing.T) {
	res := archivecompleteness.HashDBVerifyResult{
		From: 100, To: 104,
		Verified: 5,
	}
	got := classifyHashDBVerifySweep(res, nil, 100, 104)
	if got != sweepOutcomeOK {
		t.Fatalf("classifyHashDBVerifySweep() = %v, want sweepOutcomeOK", got)
	}
}

// TestClassifyHashDBVerifySweep_IncompleteWithoutError pins the
// silent-short-stream case: fewer ledgers observed than the window
// requires, no error, no drift — must not read as clean.
func TestClassifyHashDBVerifySweep_IncompleteWithoutError(t *testing.T) {
	res := archivecompleteness.HashDBVerifyResult{
		From: 100, To: 104,
		Verified: 3,
	}
	got := classifyHashDBVerifySweep(res, nil, 100, 104)
	if got != sweepOutcomeIncomplete {
		t.Fatalf("classifyHashDBVerifySweep() = %v, want sweepOutcomeIncomplete", got)
	}
}

// TestCountNewDrift_DedupesAcrossOverlappingWindows pins that consecutive sweeps with
// overlapping trailing windows count a still-drifted ledger once in HashdbDriftTotal, not
// again on every sweep.
func TestCountNewDrift_DedupesAcrossOverlappingWindows(t *testing.T) {
	seen := make(map[uint32]struct{})

	first := archivecompleteness.HashDBVerifyResult{
		Drifted:   1,
		DriftSeqs: []uint32{102},
	}
	if got := countNewDrift(first, seen); got != 1 {
		t.Fatalf("countNewDrift(first sweep) = %d, want 1 (first observation)", got)
	}

	// Second tick's window overlaps the first and re-observes ledger
	// 102 (still drifted) plus one genuinely new drifted ledger, 108.
	second := archivecompleteness.HashDBVerifyResult{
		Drifted:   2,
		DriftSeqs: []uint32{102, 108},
	}
	if got := countNewDrift(second, seen); got != 1 {
		t.Fatalf("countNewDrift(second sweep) = %d, want 1 — ledger 102 was already counted, only 108 is new", got)
	}
}

// TestCountNewDrift_NilSeenCountsRaw pins that the one-off runVerifyHashDBRange path, with no repeat observations to
// dedupe, passes the raw Drifted count through unchanged.
func TestCountNewDrift_NilSeenCountsRaw(t *testing.T) {
	res := archivecompleteness.HashDBVerifyResult{
		Drifted:   3,
		DriftSeqs: []uint32{10, 11, 12},
	}
	if got := countNewDrift(res, nil); got != 3 {
		t.Fatalf("countNewDrift(nil seen) = %d, want 3 (raw count)", got)
	}
}

// TestCountNewDrift_ExcessBeyondCapCountsRaw pins that drift beyond MaxHashDBDriftSeqsReported, which DriftSeqs cannot
// identify, is still counted rather than dropped.
func TestCountNewDrift_ExcessBeyondCapCountsRaw(t *testing.T) {
	seen := make(map[uint32]struct{})
	res := archivecompleteness.HashDBVerifyResult{
		Drifted:   5,
		DriftSeqs: []uint32{1, 2}, // capped sample, true count is 5
	}
	if got := countNewDrift(res, seen); got != 5 {
		t.Fatalf("countNewDrift(capped) = %d, want 5 (2 identified + 3 excess)", got)
	}
}

// A failing pass must count under the window it ran for.
func TestHashDBVerifyPass_CountsUnderWindowLabel(t *testing.T) {
	db, err := hashdb.Create(filepath.Join(t.TempDir(), "h.db"), 1)
	if err != nil {
		t.Fatalf("hashdb.Create: %v", err)
	}
	defer func() { _ = db.Close() }()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, window := range []string{hashDBWindowRecent} {
		c := obs.HashdbVerifyRunsTotal.WithLabelValues("error", window)
		before := testutil.ToFloat64(c)
		// Empty ledgerstream.Config: Stream fails before any read.
		hashDBVerifyPass(context.Background(), logger, db, ledgerstream.Config{}, 10, 20, nil, window)
		if got := testutil.ToFloat64(c) - before; got != 1 {
			t.Errorf("window=%s: error counter delta = %v, want 1", window, got)
		}
	}
}

// TestVerifyHashDBRangeFlags_Validate pins the -verify-hashdb-from/-to contract: neither set leaves full ingest
// unchanged, a range far behind the tip passes through, and a half-set or inverted range fails loudly with
// requested=true.
func TestVerifyHashDBRangeFlags_Validate(t *testing.T) {
	cases := []struct {
		name          string
		flags         verifyHashDBRangeFlags
		wantFrom      uint32
		wantTo        uint32
		wantRequested bool
		wantErr       bool
	}{
		{name: "neither set is not requested"},
		{name: "explicit older range is accepted", flags: verifyHashDBRangeFlags{from: 1_000_000, to: 1_000_500}, wantFrom: 1_000_000, wantTo: 1_000_500, wantRequested: true},
		{name: "only from set is rejected", flags: verifyHashDBRangeFlags{from: 100}, wantRequested: true, wantErr: true},
		{name: "only to set is rejected", flags: verifyHashDBRangeFlags{to: 100}, wantRequested: true, wantErr: true},
		{name: "from after to is rejected", flags: verifyHashDBRangeFlags{from: 200, to: 100}, wantRequested: true, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			from, to, requested, err := tc.flags.validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("validate(%+v) err = %v, wantErr %v", tc.flags, err, tc.wantErr)
			}
			if requested != tc.wantRequested {
				t.Fatalf("validate(%+v) requested = %v, want %v", tc.flags, requested, tc.wantRequested)
			}
			if !tc.wantErr && (from != tc.wantFrom || to != tc.wantTo) {
				t.Fatalf("validate(%+v) = (%d, %d), want (%d, %d)", tc.flags, from, to, tc.wantFrom, tc.wantTo)
			}
		})
	}
}
