package timescale

import (
	"testing"
	"time"
)

// TestGapDetectorStatementTimeoutWithinGoBudget pins the timeout
// ordering the 2026-08-28 r1 incident violated: the PG-side
// statement_timeout the gap detector SETs on its scan queries must sit
// at or below the Go per-target context timeout. When it doesn't (the
// count-distinct used the 2h ops/verify constant against a 15-min Go
// context), an over-budget query is abandoned by Go but keeps running
// in PG, and each subsequent cycle / restart stacks another orphan on a
// 257 GB hypertable scan — 121 calls, mean 556 s, 18.7 h of IO, load
// 19.6, 503s on the serving path.
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
