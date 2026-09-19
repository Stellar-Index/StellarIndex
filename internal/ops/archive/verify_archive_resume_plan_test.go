package archive

import (
	"os"
	"path/filepath"
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

// walkedChunk is the chunkResult a worker returns for a chunk that
// covered its whole range cleanly.
func walkedChunk(idx int, c opsutil.RangeChunk, firstPrev, last sdkxdr.Hash) chunkResult {
	return chunkResult{
		Idx:           idx,
		From:          c.From,
		To:            c.To,
		FirstSeq:      c.From,
		FirstPrevHash: firstPrev,
		LastSeq:       c.To,
		LastHash:      last,
		Verified:      int(c.To-c.From) + 1,
	}
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

// TestPlanResumedWalk_PartialResumeStillSkipsDoneChunks: neither fix
// may disable resume. A genuinely interrupted run (some chunks Done
// WITH their boundary evidence, some not) still skips the finished
// ones.
func TestPlanResumedWalk_PartialResumeStillSkipsDoneChunks(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 19, 4, 37, 0, 0, time.UTC)
	chunks := threeChunkPlan()
	st := startTierProgress(VerifyArchiveState{}, "checkpoint", 2, 3000, 3, chunks, now)
	st = markChunkDoneStitch(st, "checkpoint", 0, walkedChunk(0, chunks[0], hashByte(0x10), hashByte(0x11)), now)

	got, idxs, _ := planResumedWalk(st, "checkpoint", 2, 3000, 3, chunks)
	if len(got) != 2 || got[0] != chunks[1] || got[1] != chunks[2] {
		t.Fatalf("plan = %+v, want the two not-Done chunks %+v", got, chunks[1:])
	}
	if len(idxs) != 2 || idxs[0] != 1 || idxs[1] != 2 {
		t.Fatalf("idxs = %v, want [1 2]", idxs)
	}
}

// TestPlanResumedWalk_DoneWithoutBoundaryEvidenceIsRewalked is the
// RLT-265 admission rule: a chunk recorded Done by a binary that never
// persisted its boundary terms cannot supply either side of a
// cross-chunk boundary, so skipping it would leave that boundary
// unchecked in every run. It is re-walked instead — self-healing,
// because the re-walk records the evidence and the next resume works.
func TestPlanResumedWalk_DoneWithoutBoundaryEvidenceIsRewalked(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 19, 4, 37, 0, 0, time.UTC)
	chunks := threeChunkPlan()
	st := startTierProgress(VerifyArchiveState{}, "checkpoint", 2, 3000, 3, chunks, now)
	// markChunkDone is the pre-RLT-265 recording: Done + the terminal
	// hash, no FirstPrevHash and no verified count.
	st = markChunkDone(st, "checkpoint", 0, hashByte(0x11), now)

	got, idxs, reason := planResumedWalk(st, "checkpoint", 2, 3000, 3, chunks)
	if len(got) != len(chunks) || len(idxs) != len(chunks) {
		t.Fatalf("plan = %+v idxs = %v, want the full plan: a Done chunk with no boundary "+
			"evidence must be re-walked, not skipped (RLT-265)", got, idxs)
	}
	if !strings.Contains(reason, "no cross-chunk boundary evidence") {
		t.Errorf("reason = %q, want it to name the missing boundary evidence", reason)
	}
}

// ─── RLT-265: the cross-chunk chain proof must survive a resume ────

// TestFullPlanStitchInput_ChainBreakBesideSkippedChunkIsCaught is the
// RLT-265 regression, and its first assertion is the contrast that
// makes the second non-vacuous: the SAME stitchChunks, over the chunks
// this run walked, cannot see the break at all.
//
// Plan: three contiguous chunks. The prior run finished chunk 0; this
// run walks chunks 1 and 2. Chunks 1 and 2 ARE adjacent to each other,
// so the live-results stitch is clean — and the boundary between the
// skipped chunk 0 and chunk 1 is checked by no run, in either run.
// Plant a real chain break exactly there: the archive's ledger 1001
// does not chain onto ledger 1000.
func TestFullPlanStitchInput_ChainBreakBesideSkippedChunkIsCaught(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 19, 4, 37, 0, 0, time.UTC)
	chunks := threeChunkPlan()

	// Prior run: chunk 0 walked to ledger 1000, ending on hash 0xaa.
	chunk0 := walkedChunk(0, chunks[0], hashByte(0x01), hashByte(0xaa))
	st := startTierProgress(VerifyArchiveState{}, "chain", 2, 3000, 3, chunks, now)
	st = markChunkDoneStitch(st, "chain", 0, chunk0, now)

	// This run: chunk 1 starts at ledger 1001 whose PreviousLedgerHash
	// is 0xbb — it does NOT chain onto chunk 0's 0xaa. Chunks 1→2
	// chain cleanly, so nothing inside this run's own results is
	// wrong.
	chunk1 := walkedChunk(1, chunks[1], hashByte(0xbb), hashByte(0xcc))
	chunk2 := walkedChunk(2, chunks[2], hashByte(0xcc), hashByte(0xdd))
	live := []chunkResult{chunk1, chunk2}
	liveIdxs := []int{1, 2}

	// Contrast: what the walk used to hand stitchChunks. Adjacent to
	// each other, so clean — the break is invisible.
	if err := stitchChunks(live); err != nil {
		t.Fatalf("precondition: the live results alone must stitch clean (that is the blindness "+
			"this test is about), got %v", err)
	}

	planInput, err := fullPlanStitchInput(st, "chain", len(chunks), liveIdxs, live)
	if err != nil {
		t.Fatalf("fullPlanStitchInput: %v", err)
	}
	if len(planInput) != len(chunks) {
		t.Fatalf("stitch input has %d chunk(s), want one per plan chunk (%d)", len(planInput), len(chunks))
	}
	// The reconstructed chunk 0 must carry the prior run's terms, not
	// a zero value: a zero LastHash would compare equal to another
	// zero hash and pass.
	if planInput[0].LastSeq != 1000 || planInput[0].LastHash != hashByte(0xaa) {
		t.Errorf("reconstructed chunk[0] = LastSeq %d hash %s, want 1000 / %s",
			planInput[0].LastSeq, hashToHex(planInput[0].LastHash), hashToHex(hashByte(0xaa)))
	}
	if planInput[0].Verified != chunk0.Verified {
		t.Errorf("reconstructed chunk[0].Verified = %d, want %d — a chunk reconstructed as empty "+
			"would be skipped when stitchChunks picks pairs", planInput[0].Verified, chunk0.Verified)
	}

	stitchErr := stitchChunks(planInput)
	if stitchErr == nil {
		t.Fatal("stitchChunks over the full plan returned nil: the chain break at the boundary " +
			"beside the skipped chunk 0 is still unchecked (RLT-265)")
	}
	if !strings.Contains(stitchErr.Error(), "chunk[0→1]") {
		t.Errorf("error = %v, want it to name the chunk[0→1] boundary", stitchErr)
	}
	if !strings.Contains(stitchErr.Error(), "boundary chain break") {
		t.Errorf("error = %v, want a chain-break verdict (not a sequence gap)", stitchErr)
	}
}

// TestFullPlanStitchInput_IntactChainAcrossSkippedChunkPasses is the
// other half: a resumed walk whose chunks DO chain must not be failed.
// Pre-fix this shape produced a spurious gap — stitchChunks compared
// chunk 0 (ends 1000) against chunk 2 (starts 2001) because the
// skipped chunk 1 was simply absent from the slice.
func TestFullPlanStitchInput_IntactChainAcrossSkippedChunkPasses(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 19, 4, 37, 0, 0, time.UTC)
	chunks := threeChunkPlan()

	chunk0 := walkedChunk(0, chunks[0], hashByte(0x01), hashByte(0xaa))
	chunk1 := walkedChunk(1, chunks[1], hashByte(0xaa), hashByte(0xbb))
	chunk2 := walkedChunk(2, chunks[2], hashByte(0xbb), hashByte(0xcc))

	// Prior run finished the MIDDLE chunk.
	st := startTierProgress(VerifyArchiveState{}, "chain", 2, 3000, 3, chunks, now)
	st = markChunkDoneStitch(st, "chain", 1, chunk1, now)

	live := []chunkResult{chunk0, chunk2}
	liveIdxs := []int{0, 2}

	// Contrast: the pre-fix input reports a gap between two chunks
	// that are not adjacent, on a chain that is in fact intact.
	if err := stitchChunks(live); err == nil {
		t.Fatal("precondition: the live results alone should report a spurious boundary gap")
	}

	planInput, err := fullPlanStitchInput(st, "chain", len(chunks), liveIdxs, live)
	if err != nil {
		t.Fatalf("fullPlanStitchInput: %v", err)
	}
	if err := stitchChunks(planInput); err != nil {
		t.Fatalf("stitchChunks over the full plan failed on an intact chain: %v", err)
	}
	if planInput[1].FirstSeq != 1001 || planInput[1].FirstPrevHash != hashByte(0xaa) {
		t.Errorf("reconstructed chunk[1] = FirstSeq %d prevHash %s, want 1001 / %s",
			planInput[1].FirstSeq, hashToHex(planInput[1].FirstPrevHash), hashToHex(hashByte(0xaa)))
	}
}

// TestFullPlanStitchInput_RefusesAHoleInThePlan: a skipped chunk with
// no persisted evidence must be an error, never a silently shortened
// plan. planResumedWalk does not produce this input, so it is the
// defensive edge.
func TestFullPlanStitchInput_RefusesAHoleInThePlan(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 19, 4, 37, 0, 0, time.UTC)
	chunks := threeChunkPlan()
	st := startTierProgress(VerifyArchiveState{}, "chain", 2, 3000, 3, chunks, now)
	st = markChunkDone(st, "chain", 0, hashByte(0xaa), now) // no Stitch record

	live := []chunkResult{
		walkedChunk(1, chunks[1], hashByte(0xaa), hashByte(0xbb)),
		walkedChunk(2, chunks[2], hashByte(0xbb), hashByte(0xcc)),
	}
	if _, err := fullPlanStitchInput(st, "chain", len(chunks), []int{1, 2}, live); err == nil {
		t.Fatal("fullPlanStitchInput accepted a plan with an unreconstructable chunk; want an error")
	}
}

// TestVerifyArchiveState_StitchSurvivesTheStateFile writes and reads
// the real state file. The boundary evidence is only useful if it
// survives the process boundary it exists to cross — the writer and
// the reader are the two halves of one loop, and neither half proves
// the other.
//
// It also pins backward compatibility in both directions: a state file
// written before the field existed must read back with Stitch nil (the
// shape planResumedWalk re-walks), not as a zero-valued record that
// would be treated as evidence.
func TestVerifyArchiveState_StitchSurvivesTheStateFile(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 19, 4, 37, 0, 0, time.UTC)
	chunks := threeChunkPlan()
	st := startTierProgress(VerifyArchiveState{}, "chain", 2, 3000, 3, chunks, now)
	st = markChunkDoneStitch(st, "chain", 0, walkedChunk(0, chunks[0], hashByte(0x01), hashByte(0xaa)), now)

	path := filepath.Join(t.TempDir(), "verify-archive-state.json")
	if err := writeVerifyArchiveState(path, st); err != nil {
		t.Fatalf("write state: %v", err)
	}
	got, err := readVerifyArchiveState(path)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	rp := got.Tiers["chain"].InProgress
	if rp == nil || len(rp.Chunks) != 3 {
		t.Fatalf("in-progress lost across the state file: %+v", rp)
	}
	s := rp.Chunks[0].Stitch
	if s == nil {
		t.Fatal("chunk[0].Stitch did not survive the state file — a resumed run would have no " +
			"boundary evidence and would re-walk every night (RLT-265)")
	}
	if s.FirstSeq != 2 || s.LastSeq != 1000 || s.Verified != 999 {
		t.Errorf("stitch record = %+v, want first_seq 2 / last_seq 1000 / verified 999", *s)
	}
	if s.FirstPrevHash != hashToHex(hashByte(0x01)) || s.LastHash != hashToHex(hashByte(0xaa)) {
		t.Errorf("stitch hashes = %s / %s, want %s / %s",
			s.FirstPrevHash, s.LastHash, hashToHex(hashByte(0x01)), hashToHex(hashByte(0xaa)))
	}
	// Round-trips through fullPlanStitchInput, which is what actually
	// consumes it — a writer proved only against itself proves little.
	planInput, err := fullPlanStitchInput(got, "chain", 3, []int{1, 2}, []chunkResult{
		walkedChunk(1, chunks[1], hashByte(0xaa), hashByte(0xbb)),
		walkedChunk(2, chunks[2], hashByte(0xbb), hashByte(0xcc)),
	})
	if err != nil {
		t.Fatalf("fullPlanStitchInput over the re-read state: %v", err)
	}
	if err := stitchChunks(planInput); err != nil {
		t.Fatalf("stitch over the re-read state failed on an intact chain: %v", err)
	}

	// A pre-RLT-265 state file has no "stitch" key at all.
	legacy := `{"tiers":{"chain":{"last_verified_ledger":1000,"in_progress":` +
		`{"from":2,"to":3000,"workers":3,"chunks":[{"idx":0,"from":2,"to":1000,"done":true,` +
		`"last_verified_hash":"` + hashToHex(hashByte(0xaa)) + `"}]}}}}`
	legacyPath := filepath.Join(t.TempDir(), "legacy.json")
	if err := os.WriteFile(legacyPath, []byte(legacy), 0o600); err != nil {
		t.Fatalf("write legacy state: %v", err)
	}
	legacyState, err := readVerifyArchiveState(legacyPath)
	if err != nil {
		t.Fatalf("read legacy state: %v", err)
	}
	if got := legacyState.Tiers["chain"].InProgress.Chunks[0].Stitch; got != nil {
		t.Errorf("legacy chunk read back with Stitch = %+v, want nil so planResumedWalk re-walks it", *got)
	}
}

// TestChunkStitchRoundTrip: a corrupt or truncated persisted hash must
// be an error, not a zero hash — two zero hashes compare equal and
// would turn a corrupt state file into a passing chain proof.
func TestChunkStitchRoundTrip(t *testing.T) {
	t.Parallel()
	cp := ChunkProgress{Idx: 0, From: 2, To: 1000}
	good := ChunkStitch{
		Verified:      999,
		FirstSeq:      2,
		FirstPrevHash: hashToHex(hashByte(0x01)),
		LastSeq:       1000,
		LastHash:      hashToHex(hashByte(0xaa)),
	}
	res, err := good.chunkResult(0, cp)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if res.FirstPrevHash != hashByte(0x01) || res.LastHash != hashByte(0xaa) {
		t.Errorf("round trip lost the hashes: %+v", res)
	}

	bad := good
	bad.LastHash = "abcd"
	if _, err := bad.chunkResult(0, cp); err == nil {
		t.Error("a truncated persisted hash was accepted; want an error rather than a zero hash")
	}

	// A legitimately empty chunk carries no hashes and must
	// reconstruct as empty rather than erroring.
	empty := ChunkStitch{Verified: 0}
	emptyRes, err := empty.chunkResult(1, cp)
	if err != nil {
		t.Fatalf("empty chunk: %v", err)
	}
	if emptyRes.Verified != 0 {
		t.Errorf("empty chunk reconstructed with Verified=%d", emptyRes.Verified)
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

// TestChunkProgressLastVerifiedHash_IsNotTheBoundaryProof pins the
// corrected doc claim on ChunkProgress.LastVerifiedHash, which said it
// was "used for the cross-run chain-continuity proof" while no code
// read it (RLT-265). Which field is load-bearing has to be mechanical,
// not a comment: an operator debugging a boundary failure who edits
// the wrong field gets no signal at all.
//
// Poisoning LastVerifiedHash must change nothing — it is a
// human-readable mirror. Poisoning Stitch.LastHash must break the
// stitch, because that is the term the proof reads.
func TestChunkProgressLastVerifiedHash_IsNotTheBoundaryProof(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 19, 4, 37, 0, 0, time.UTC)
	chunks := threeChunkPlan()

	// chunk 0 ends at 1000 on 0xaa; chunk 1 chains onto it cleanly.
	chunk0 := walkedChunk(0, chunks[0], hashByte(0x01), hashByte(0xaa))
	chunk1 := walkedChunk(1, chunks[1], hashByte(0xaa), hashByte(0xcc))
	chunk2 := walkedChunk(2, chunks[2], hashByte(0xcc), hashByte(0xdd))
	live := []chunkResult{chunk1, chunk2}
	liveIdxs := []int{1, 2}

	base := startTierProgress(VerifyArchiveState{}, "chain", 2, 3000, 3, chunks, now)
	base = markChunkDoneStitch(base, "chain", 0, chunk0, now)

	// Precondition: the field the old comment named is in fact written.
	if got := base.Tiers["chain"].InProgress.Chunks[0].LastVerifiedHash; got != hashToHex(hashByte(0xaa)) {
		t.Fatalf("precondition: chunk[0].LastVerifiedHash = %q, want the terminal hash", got)
	}

	// Poisoning the mirror must not move the verdict.
	poisonedMirror := markChunkDoneStitch(base, "chain", 0, chunk0, now)
	poisonedMirror.Tiers["chain"].InProgress.Chunks[0].LastVerifiedHash = hashToHex(hashByte(0xff))
	planInput, err := fullPlanStitchInput(poisonedMirror, "chain", len(chunks), liveIdxs, live)
	if err != nil {
		t.Fatalf("fullPlanStitchInput with a poisoned LastVerifiedHash: %v", err)
	}
	if planInput[0].LastHash != hashByte(0xaa) {
		t.Errorf("reconstructed chunk[0].LastHash = %s, want %s — the proof must read Stitch, "+
			"not the LastVerifiedHash mirror (RLT-265)",
			hashToHex(planInput[0].LastHash), hashToHex(hashByte(0xaa)))
	}
	if err := stitchChunks(planInput); err != nil {
		t.Errorf("stitchChunks failed on an intact chain because LastVerifiedHash was poisoned: %v", err)
	}

	// Poisoning the term the proof DOES read must break it — otherwise
	// the assertion above would hold for a stitch that reads nothing.
	poisonedProof := markChunkDoneStitch(base, "chain", 0, chunk0, now)
	poisonedProof.Tiers["chain"].InProgress.Chunks[0].Stitch.LastHash = hashToHex(hashByte(0xff))
	planInput, err = fullPlanStitchInput(poisonedProof, "chain", len(chunks), liveIdxs, live)
	if err != nil {
		t.Fatalf("fullPlanStitchInput with a poisoned Stitch.LastHash: %v", err)
	}
	if err := stitchChunks(planInput); err == nil {
		t.Fatal("stitchChunks passed with chunk[0]'s persisted boundary hash poisoned — " +
			"Stitch.LastHash is not being read, so the proof is vacuous")
	}
}
