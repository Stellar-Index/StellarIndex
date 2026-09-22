package main

import (
	"context"
	"errors"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/archivecompleteness"
)

// TestClassifyHashDBVerifySweep_DriftSurvivesShutdownCancel is T128:
// a sweep that tallied drift before the stream was cancelled (e.g. by
// process shutdown) must still be classified as drift, not silently
// discarded as "nothing to report". Before the fix, the Canceled-error
// early return in hashDBVerifySweep ran unconditionally, ahead of the
// AnyDrift() check, and threw the drift away.
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
