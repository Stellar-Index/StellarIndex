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
