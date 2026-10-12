package timescale

import (
	"testing"
	"time"
)

// TestGapDetectorStatementTimeoutWithinGoBudget: the PG statement_timeout set on gap-detector scans must not
// exceed the Go per-target timeout, or Go abandons a query PG keeps running and orphan scans stack up each
// cycle.
func TestGapDetectorStatementTimeoutWithinGoBudget(t *testing.T) {
	t.Parallel()

	pgTimeout := time.Duration(gapDetectorStatementTimeoutMS) * time.Millisecond
	if pgTimeout <= 0 {
		t.Fatalf("gapDetectorStatementTimeoutMS = %d; must be positive (0 disables the PG-side backstop entirely)", gapDetectorStatementTimeoutMS)
	}
	if pgTimeout > gapDetectorPerTargetTimeout {
		t.Fatalf("gapDetectorStatementTimeoutMS = %v exceeds the Go per-target timeout %v — an over-budget scan outlives its context as an orphaned PG backend",
			pgTimeout, gapDetectorPerTargetTimeout)
	}
	// The ops/verify constant is the wrong tool here by construction:
	// it is sized for trusted multi-hour jobs whose caller context is
	// the real bound. Guard against someone "simplifying" back to it.
	if opsTimeout := time.Duration(opsVerifyStatementTimeoutMS) * time.Millisecond; opsTimeout <= gapDetectorPerTargetTimeout {
		t.Logf("note: opsVerifyStatementTimeoutMS (%v) is now within the detector budget; this guard is moot", opsTimeout)
	} else if gapDetectorStatementTimeoutMS == opsVerifyStatementTimeoutMS {
		t.Fatalf("gap detector must not share opsVerifyStatementTimeoutMS (%v) — that is the 2026-08-28 inversion", opsTimeout)
	}
}
