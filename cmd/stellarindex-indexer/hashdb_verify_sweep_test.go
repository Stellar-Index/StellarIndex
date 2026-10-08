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

// TestClassifyHashDBVerifySweep_DriftSurvivesShutdownCancel pins that a
// sweep that tallied drift before the stream was cancelled (e.g. by
// process shutdown) must still be classified as drift, not silently
// discarded as "nothing to report". The Canceled-error
// early return in hashDBVerifySweep must not run ahead of the
// AnyDrift() check, or it throws the drift away.
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

// TestCountNewDrift_DedupesAcrossOverlappingWindows is Q109: two
// consecutive periodic sweeps whose trailing windows overlap (the
// normal case — see startHashDBVerifier's ticker comment) both
// observe the same still-drifted ledger. Without dedup, the second
// sweep would re-add it to HashdbDriftTotal even though it is not a
// newly discovered drifted ledger.
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

// TestCountNewDrift_NilSeenCountsRaw pins the one-off
// runVerifyHashDBRange CLI path: a single explicit pass has no
// repeat-observation problem to dedupe, so the raw Drifted count
// passes through unchanged.
func TestCountNewDrift_NilSeenCountsRaw(t *testing.T) {
	res := archivecompleteness.HashDBVerifyResult{
		Drifted:   3,
		DriftSeqs: []uint32{10, 11, 12},
	}
	if got := countNewDrift(res, nil); got != 3 {
		t.Fatalf("countNewDrift(nil seen) = %d, want 3 (raw count)", got)
	}
}

// TestCountNewDrift_ExcessBeyondCapCountsRaw pins the capped-DriftSeqs
// edge case: Drifted can exceed len(DriftSeqs) once
// MaxHashDBDriftSeqsReported is hit. The un-identifiable excess must
// still be counted rather than silently dropped.
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

func TestPickHashDBHistorySlice(t *testing.T) {
	t.Run("no history", func(t *testing.T) {
		for _, c := range [][2]uint32{{100, 100}, {100, 50}} {
			if _, _, ok := pickHashDBHistorySlice(c[0], c[1], 1000, 1); ok {
				t.Fatalf("start=%d recentFrom=%d: want no slice", c[0], c[1])
			}
		}
		if _, _, ok := pickHashDBHistorySlice(100, 5000, 0, 1); ok {
			t.Fatal("n=0: want no slice")
		}
	})
	t.Run("history smaller than n returns all of it", func(t *testing.T) {
		from, to, ok := pickHashDBHistorySlice(100, 150, 1000, 7)
		if !ok || from != 100 || to != 149 {
			t.Fatalf("got %d..%d ok=%v, want 100..149", from, to, ok)
		}
	})
	t.Run("bounds hold over many seeds", func(t *testing.T) {
		const start, recentFrom, n = 1000, 9000, 1000
		for seed := uint64(0); seed < 5000; seed++ {
			from, to, ok := pickHashDBHistorySlice(start, recentFrom, n, seed)
			if !ok || from < start || to >= recentFrom || to-from+1 != n {
				t.Fatalf("seed %d: got %d..%d ok=%v", seed, from, to, ok)
			}
			if f2, t2, _ := pickHashDBHistorySlice(start, recentFrom, n, seed); f2 != from || t2 != to {
				t.Fatalf("seed %d not reproducible", seed)
			}
		}
	})
}

// A failing pass must count under the window it ran for, so the
// history slice's errors stay visible apart from the recent window's.
func TestHashDBVerifyPass_CountsUnderWindowLabel(t *testing.T) {
	db, err := hashdb.Create(filepath.Join(t.TempDir(), "h.db"), 1)
	if err != nil {
		t.Fatalf("hashdb.Create: %v", err)
	}
	defer func() { _ = db.Close() }()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, window := range []string{hashDBWindowRecent, hashDBWindowHistory} {
		c := obs.HashdbVerifyRunsTotal.WithLabelValues("error", window)
		before := testutil.ToFloat64(c)
		// Empty ledgerstream.Config: Stream fails before any read.
		hashDBVerifyPass(context.Background(), logger, db, ledgerstream.Config{}, 10, 20, nil, window)
		if got := testutil.ToFloat64(c) - before; got != 1 {
			t.Errorf("window=%s: error counter delta = %v, want 1", window, got)
		}
	}
}
