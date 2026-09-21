// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import "testing"

// TestClassicMovementsResumeStart_WidenedFromNotSkipped is Q216: a
// prior, narrower run wrote data only at the TOP of a later, WIDER
// -from range. MaxAccountMovementLedger over the wider range still
// finds that prior run's tip, but MinAccountMovementLedger proves the
// wider range's data does NOT start at the new -from — so the resume
// jump must be refused and the run must start at -from, or the
// newly-widened earlier region is silently never revisited.
func TestClassicMovementsResumeStart_WidenedFromNotSkipped(t *testing.T) {
	const (
		widenedFrom  = uint32(500_000) // this run's -from, widened below the prior run's start
		priorRunFrom = uint32(1_000_000)
		priorRunTip  = uint32(1_999_999) // MaxAccountMovementLedger([widenedFrom, to]) — prior run's tip, still the max in the wider range
	)

	resumeAt, jumped := classicMovementsResumeStart(widenedFrom, priorRunTip, priorRunFrom, true)

	if jumped {
		t.Fatalf("classicMovementsResumeStart(%d, %d, %d, true) = jumped=true resumeAt=%d — must NOT jump: [%d,%d) was never processed by the prior run and would be silently skipped",
			widenedFrom, priorRunTip, priorRunFrom, resumeAt, widenedFrom, priorRunFrom)
	}
	if resumeAt != widenedFrom {
		t.Fatalf("classicMovementsResumeStart(%d, %d, %d, true) resumeAt = %d, want %d (the resume must fall back to -from, not the prior run's tip)",
			widenedFrom, priorRunTip, priorRunFrom, resumeAt, widenedFrom)
	}
}

// TestClassicMovementsResumeStart_ContiguousFromStartJumps is the
// legitimate resume case: data in [from,to] genuinely starts AT from
// (the same -from as the run that wrote it), so jumping to maxLedger
// is sound per the ledger-ordered-insert invariant.
func TestClassicMovementsResumeStart_ContiguousFromStartJumps(t *testing.T) {
	const (
		from    = uint32(1_000_000)
		maxLdgr = uint32(1_999_999)
	)

	resumeAt, jumped := classicMovementsResumeStart(from, maxLdgr, from, true)

	if !jumped {
		t.Fatalf("classicMovementsResumeStart(%d, %d, %d, true) = jumped=false — want a jump: data starts exactly at -from, so the max is a sound resume point", from, maxLdgr, from)
	}
	if resumeAt != maxLdgr {
		t.Fatalf("classicMovementsResumeStart(%d, %d, %d, true) resumeAt = %d, want %d", from, maxLdgr, from, resumeAt, maxLdgr)
	}
}

// TestClassicMovementsResumeStart_NoDataInRange covers minFound=false
// (nothing written yet in [from,to] at all) — must not jump.
func TestClassicMovementsResumeStart_NoDataInRange(t *testing.T) {
	resumeAt, jumped := classicMovementsResumeStart(1_000_000, 1_999_999, 0, false)
	if jumped || resumeAt != 1_000_000 {
		t.Fatalf("classicMovementsResumeStart with minFound=false = (resumeAt=%d, jumped=%v), want (1000000, false)", resumeAt, jumped)
	}
}
