package archive

import (
	"strings"
	"testing"
	"time"

	sdkxdr "github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
)

// threeChunkPlan is the shape the nightly units produce in miniature:
// a contiguous, gapless split of [2, 3000] across three workers.
func threeChunkPlan() []opsutil.RangeChunk {
	return []opsutil.RangeChunk{
		{From: 2, To: 1000},
		{From: 1001, To: 2000},
		{From: 2001, To: 3000},
	}
}

func hashByte(b byte) sdkxdr.Hash {
	var h sdkxdr.Hash
	for i := range h {
		h[i] = b
	}
	return h
}

// TestPlanResumedWalk_AllDoneRewalksRatherThanCertifying is the
// RLT-281 regression.
//
// A prior run that marked every chunk Done and still left InProgress
// behind is a run that failed AT OR AFTER the post-walk proofs — the
// cross-chunk stitch and the checkpoint-anchor decision run after the
// last chunk is marked Done, and a real boundary chain break leaves
// exactly this state. verifyArchiveLCMWalk used to read resumeChunks'
// nil verdict directly and `return 0, "", nil`: a zero-ledger success.
// Its caller's textfile defer keys on retErr == nil, so
// stellarindex_verify_archive_last_success_unix advanced for a run
// that anchored nothing, holding the run-stale page green, and
// updateTierState cleared the InProgress record that was the only
// remaining trace of the failed run.
//
// The corrected value is the FULL plan: every chunk, in plan order.
func TestPlanResumedWalk_AllDoneRewalksRatherThanCertifying(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 19, 4, 37, 0, 0, time.UTC)
	chunks := threeChunkPlan()
	st := startTierProgress(VerifyArchiveState{}, "checkpoint", 2, 3000, 3, chunks, now)
	for i := range chunks {
		st = markChunkDone(st, "checkpoint", i, hashByte(byte(i+1)), now)
	}

	// Precondition: this is genuinely the all-Done state, i.e. the
	// input on which the pre-fix caller returned a zero-ledger
	// success. Without this the test could pass vacuously on a state
	// that never reached the defective branch.
	if keep, _, reason := resumeChunks(st, "checkpoint", 2, 3000, 3, chunks); keep != nil {
		t.Fatalf("precondition: want the all-Done verdict from resumeChunks, got %d chunk(s) (%s)", len(keep), reason)
	}

	got, idxs, reason := planResumedWalk(st, "checkpoint", 2, 3000, 3, chunks)
	if len(got) != len(chunks) {
		t.Fatalf("planResumedWalk returned %d chunk(s) to walk, want the full plan of %d: "+
			"an all-Done prior run must be re-walked, not certified as a zero-ledger success (RLT-281)",
			len(got), len(chunks))
	}
	for i := range chunks {
		if got[i] != chunks[i] {
			t.Errorf("plan[%d] = %+v, want %+v", i, got[i], chunks[i])
		}
		if idxs[i] != i {
			t.Errorf("idxs[%d] = %d, want %d — the original chunk index drives the state-file Done marker", i, idxs[i], i)
		}
	}
	if !strings.Contains(reason, "re-walking the full plan") {
		t.Errorf("reason = %q, want it to tell the operator the full plan is being re-walked", reason)
	}
}

// TestPlanResumedWalk_PartialResumeStillSkipsDoneChunks: the RLT-281
// fix must not disable resume. A genuinely interrupted run (some
// chunks Done, some not) still skips the finished ones.
func TestPlanResumedWalk_PartialResumeStillSkipsDoneChunks(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 19, 4, 37, 0, 0, time.UTC)
	chunks := threeChunkPlan()
	st := startTierProgress(VerifyArchiveState{}, "checkpoint", 2, 3000, 3, chunks, now)
	st = markChunkDone(st, "checkpoint", 0, hashByte(0x11), now)

	got, idxs, _ := planResumedWalk(st, "checkpoint", 2, 3000, 3, chunks)
	if len(got) != 2 || got[0] != chunks[1] || got[1] != chunks[2] {
		t.Fatalf("plan = %+v, want the two not-Done chunks %+v", got, chunks[1:])
	}
	if len(idxs) != 2 || idxs[0] != 1 || idxs[1] != 2 {
		t.Fatalf("idxs = %v, want [1 2]", idxs)
	}
}

// TestPlanResumedWalk_NoPriorStateWalksEverything: the cold-start and
// plan-mismatch paths are unchanged.
func TestPlanResumedWalk_NoPriorStateWalksEverything(t *testing.T) {
	t.Parallel()
	chunks := threeChunkPlan()
	got, idxs, reason := planResumedWalk(VerifyArchiveState{}, "checkpoint", 2, 3000, 3, chunks)
	if len(got) != 3 || len(idxs) != 3 {
		t.Fatalf("plan = %+v idxs = %v, want all three chunks", got, idxs)
	}
	if !strings.Contains(reason, "no prior in-progress") {
		t.Errorf("reason = %q, want the cold-start reason", reason)
	}
}
