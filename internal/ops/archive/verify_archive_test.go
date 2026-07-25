package archive

import (
	"strings"
	"testing"
)

// TestCheckpointAnchorDecision_AllMissedFailsRegardlessOfFlag is the
// DAT-09 regression: checkpointsOK==0 && checkpointsMissed>0 (every
// checkpoint anchor missed) must return a non-nil error even when
// -fail-on-missed is NOT set — an all-missed range verified nothing
// against the cross-anchor archive and must not be certified
// complete or advance the checkpoint tier's high-water mark.
func TestCheckpointAnchorDecision_AllMissedFailsRegardlessOfFlag(t *testing.T) {
	for _, failOnMissed := range []bool{false, true} {
		err := checkpointAnchorDecision(0, 5, failOnMissed)
		if err == nil {
			t.Fatalf("failOnMissed=%v: expected a non-nil error when checkpointsOK=0 checkpointsMissed=5, got nil", failOnMissed)
		}
		if !strings.Contains(err.Error(), "inconclusive") {
			t.Errorf("failOnMissed=%v: expected 'inconclusive' in error, got: %v", failOnMissed, err)
		}
	}
}

// TestCheckpointAnchorDecision_AllMatchedIsClean: every checkpoint
// matched — clean regardless of failOnMissed.
func TestCheckpointAnchorDecision_AllMatchedIsClean(t *testing.T) {
	for _, failOnMissed := range []bool{false, true} {
		if err := checkpointAnchorDecision(10, 0, failOnMissed); err != nil {
			t.Errorf("failOnMissed=%v: expected nil error when all checkpoints matched, got %v", failOnMissed, err)
		}
	}
}

// TestCheckpointAnchorDecision_PartialMiss: SOME matched, some
// missed — the pre-existing -fail-on-missed gate applies (distinct
// from the DAT-09 all-missed case).
func TestCheckpointAnchorDecision_PartialMiss(t *testing.T) {
	if err := checkpointAnchorDecision(8, 2, false); err != nil {
		t.Errorf("partial miss without -fail-on-missed should be clean, got %v", err)
	}
	err := checkpointAnchorDecision(8, 2, true)
	if err == nil {
		t.Fatal("partial miss WITH -fail-on-missed should fail, got nil")
	}
	if !strings.Contains(err.Error(), "fail-on-missed") {
		t.Errorf("expected 'fail-on-missed' in error, got: %v", err)
	}
}

// TestCheckpointAnchorDecision_NoCheckpointsAttempted: 0/0 (the
// doCheckpoint gate wasn't even reached, or the range genuinely had
// zero checkpoint positions upstream) must not be treated as the
// all-missed failure — checkpointsMissed must be > 0 to trigger it.
func TestCheckpointAnchorDecision_NoCheckpointsAttempted(t *testing.T) {
	if err := checkpointAnchorDecision(0, 0, false); err != nil {
		t.Errorf("0 matched / 0 missed should not trip the all-missed guard, got %v", err)
	}
}
