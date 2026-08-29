package archive

import (
	"path/filepath"
	"strings"
	"testing"

	sdkxdr "github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
)

// TestSplitRange_VariousSizes — pinning the split semantics across
// the corner cases the chunk orchestrator can hit. Final-chunk
// absorbs-remainder is the load-bearing property; tests check both
// the count and the boundary contiguity invariant
// (chunks[i].to + 1 == chunks[i+1].from).
func TestSplitRange_VariousSizes(t *testing.T) {
	tests := []struct {
		name      string
		from, to  uint32
		workers   int
		wantCount int
		wantFirst uint32
		wantLast  uint32
	}{
		{"workers=1 yields one chunk", 100, 200, 1, 1, 100, 200},
		{"workers=0 collapses to 1", 100, 200, 0, 1, 100, 200},
		{"even split", 100, 199, 4, 4, 100, 199},                  // 25 each
		{"uneven split — last absorbs", 100, 200, 4, 4, 100, 200}, // 25/25/25/26
		{"workers > span", 100, 102, 5, 1, 100, 102},
		{"single ledger range", 100, 100, 4, 1, 100, 100},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := opsutil.SplitRange(tc.from, tc.to, tc.workers)
			if len(got) != tc.wantCount {
				t.Fatalf("got %d chunks, want %d (chunks=%v)", len(got), tc.wantCount, got)
			}
			if got[0].From != tc.wantFirst {
				t.Errorf("got[0].From = %d, want %d", got[0].From, tc.wantFirst)
			}
			if got[len(got)-1].To != tc.wantLast {
				t.Errorf("got[last].to = %d, want %d", got[len(got)-1].To, tc.wantLast)
			}
			// Contiguity invariant.
			for i := 0; i < len(got)-1; i++ {
				if got[i].To+1 != got[i+1].From {
					t.Errorf("gap between chunk[%d].to=%d and chunk[%d].from=%d",
						i, got[i].To, i+1, got[i+1].From)
				}
			}
		})
	}
}

// hashFrom builds a deterministic Hash for tests so we can express
// "ledger N's hash" in a compact form.
func hashFrom(b byte) sdkxdr.Hash {
	var h sdkxdr.Hash
	h[0] = b
	return h
}

// TestStitchChunks_SingleChunkPasses — a single chunk has no
// boundary to check; stitch must succeed.
func TestStitchChunks_SingleChunkPasses(t *testing.T) {
	results := []chunkResult{
		{Idx: 0, FirstSeq: 100, LastSeq: 199, FirstPrevHash: hashFrom(0x99), LastHash: hashFrom(0xCC), Verified: 100},
	}
	if err := stitchChunks(results); err != nil {
		t.Errorf("single-chunk stitch should succeed; got %v", err)
	}
}

// TestStitchChunks_HappyPath — adjacent chunks where chunk[i].
// LastHash matches chunk[i+1].FirstPrevHash AND seqs are
// contiguous → no error.
func TestStitchChunks_HappyPath(t *testing.T) {
	results := []chunkResult{
		{Idx: 0, FirstSeq: 100, LastSeq: 199, LastHash: hashFrom(0xAA), Verified: 100},
		{Idx: 1, FirstSeq: 200, FirstPrevHash: hashFrom(0xAA), LastSeq: 299, LastHash: hashFrom(0xBB), Verified: 100},
		{Idx: 2, FirstSeq: 300, FirstPrevHash: hashFrom(0xBB), LastSeq: 399, LastHash: hashFrom(0xCC), Verified: 100},
	}
	if err := stitchChunks(results); err != nil {
		t.Errorf("happy-path stitch failed: %v", err)
	}
}

// TestStitchChunks_HashMismatch — chunk[1].FirstPrevHash differs
// from chunk[0].LastHash → error names both chunks + ledger.
func TestStitchChunks_HashMismatch(t *testing.T) {
	results := []chunkResult{
		{Idx: 0, FirstSeq: 100, LastSeq: 199, LastHash: hashFrom(0xAA), Verified: 100},
		{Idx: 1, FirstSeq: 200, FirstPrevHash: hashFrom(0xDD), LastSeq: 299, LastHash: hashFrom(0xBB), Verified: 100},
	}
	err := stitchChunks(results)
	if err == nil {
		t.Fatal("expected boundary-mismatch error; got nil")
	}
	for _, want := range []string{"chunk[0→1]", "chain break", "ledger 199"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error message missing %q: %v", want, err)
		}
	}
}

// TestStitchChunks_SeqGap — chunk[0].LastSeq + 1 != chunk[1].FirstSeq
// (a missing ledger between chunks) → error.
func TestStitchChunks_SeqGap(t *testing.T) {
	results := []chunkResult{
		{Idx: 0, FirstSeq: 100, LastSeq: 199, LastHash: hashFrom(0xAA), Verified: 100},
		{Idx: 1, FirstSeq: 250, FirstPrevHash: hashFrom(0xAA), LastSeq: 299, LastHash: hashFrom(0xBB), Verified: 50},
	}
	err := stitchChunks(results)
	if err == nil {
		t.Fatal("expected seq-gap error; got nil")
	}
	if !strings.Contains(err.Error(), "boundary gap") {
		t.Errorf("error message lacks 'boundary gap': %v", err)
	}
}

// TestCheckResumeFromHash_HappyPath — operator-supplied hex matches
// the observed FirstPrevHash → no error.
func TestCheckResumeFromHash_HappyPath(t *testing.T) {
	h := hashFrom(0xAA)
	hex := "aa" + strings.Repeat("00", 31)
	if err := checkResumeFromHash(hex, h, 100); err != nil {
		t.Errorf("happy path failed: %v", err)
	}
}

// TestCheckResumeFromHash_Mismatch — operator passed a hash that
// doesn't match the observed FirstPrevHash; error names the seam
// ledger + both hashes so the operator can audit.
func TestCheckResumeFromHash_Mismatch(t *testing.T) {
	h := hashFrom(0xAA)
	wrongHex := "bb" + strings.Repeat("00", 31)
	err := checkResumeFromHash(wrongHex, h, 100)
	if err == nil {
		t.Fatal("expected mismatch error")
	}
	for _, want := range []string{"ledger 100", "boundary mismatch", "FirstPrevHash"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error message lacks %q: %v", want, err)
		}
	}
}

// TestCheckResumeFromHash_BadHex — operator typo (non-hex chars,
// wrong length) surfaces as a parse error rather than silently
// passing.
func TestCheckResumeFromHash_BadHex(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"non-hex chars", "zzznotahex"},
		{"odd length", "aab"},
		{"too short", "aabb"},
		{"too long", strings.Repeat("aa", 33)}, // 33 bytes = 66 chars
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := checkResumeFromHash(tc.in, hashFrom(0xAA), 100); err == nil {
				t.Errorf("expected error for %q; got nil", tc.in)
			}
		})
	}
}

// TestStitchChunks_EmptyChunkInMiddleWithRealGapErrors is the DAT-11
// regression: a chunk that processed zero ledgers is excluded from
// the pairwise comparison, but the check is re-targeted at the
// nearest non-empty neighbours on EITHER side — it must NOT be
// skipped outright. Here chunk[0].LastSeq=199 and chunk[2].FirstSeq=
// 300 (a real 100-ledger gap the empty middle chunk was supposed to
// cover but verified nothing in), so stitching the surrounding
// non-empty chunks must surface it as a boundary gap.
func TestStitchChunks_EmptyChunkInMiddleWithRealGapErrors(t *testing.T) {
	results := []chunkResult{
		{Idx: 0, FirstSeq: 100, LastSeq: 199, LastHash: hashFrom(0xAA), Verified: 100},
		{Idx: 1, Verified: 0}, // empty chunk — masked a real gap in the old behaviour
		{Idx: 2, FirstSeq: 300, FirstPrevHash: hashFrom(0xBB), LastSeq: 399, LastHash: hashFrom(0xCC), Verified: 100},
	}
	err := stitchChunks(results)
	if err == nil {
		t.Fatal("expected a boundary-gap error surfaced across the empty middle chunk; got nil")
	}
	if !strings.Contains(err.Error(), "boundary gap") {
		t.Errorf("expected 'boundary gap', got: %v", err)
	}
	// Must name the actual surrounding chunk indices (0 and 2), not
	// the skipped empty one (1).
	if !strings.Contains(err.Error(), "chunk[0") || !strings.Contains(err.Error(), "2]") {
		t.Errorf("expected the error to name chunks 0 and 2 (surrounding the empty chunk 1): %v", err)
	}
}

// TestStitchChunks_EmptyChunkInMiddleNoRealGapPasses: when the
// surrounding non-empty chunks DO connect cleanly across the empty
// middle chunk (the empty chunk's range genuinely had zero LCMs, not
// a hole), the stitch still passes — the fix must not turn every
// legitimate empty chunk into a false failure.
func TestStitchChunks_EmptyChunkInMiddleNoRealGapPasses(t *testing.T) {
	results := []chunkResult{
		{Idx: 0, FirstSeq: 100, LastSeq: 199, LastHash: hashFrom(0xAA), Verified: 100},
		{Idx: 1, Verified: 0}, // empty chunk, but no real gap
		{Idx: 2, FirstSeq: 200, FirstPrevHash: hashFrom(0xAA), LastSeq: 299, LastHash: hashFrom(0xBB), Verified: 100},
	}
	if err := stitchChunks(results); err != nil {
		t.Errorf("empty middle chunk with a clean surrounding boundary should not stitch-error; got %v", err)
	}
}

// TestStitchChunks_AllEmptyPasses: every chunk empty (e.g. -from/-to
// entirely before any bucket exists) has no non-empty pair to check
// — passes trivially.
func TestStitchChunks_AllEmptyPasses(t *testing.T) {
	results := []chunkResult{
		{Idx: 0, Verified: 0},
		{Idx: 1, Verified: 0},
	}
	if err := stitchChunks(results); err != nil {
		t.Errorf("all-empty chunks should not stitch-error; got %v", err)
	}
}

// TestStitchChunks_TrailingEmptyChunkPasses: an empty chunk at the
// END with no non-empty chunk after it has nothing to compare
// against — passes.
func TestStitchChunks_TrailingEmptyChunkPasses(t *testing.T) {
	results := []chunkResult{
		{Idx: 0, FirstSeq: 100, LastSeq: 199, LastHash: hashFrom(0xAA), Verified: 100},
		{Idx: 1, Verified: 0},
	}
	if err := stitchChunks(results); err != nil {
		t.Errorf("trailing empty chunk should not stitch-error; got %v", err)
	}
}

// ─── #282 (repair): a boundary divergence must be PAGEABLE ─────────

// TestStitchChunks_BoundaryBreakIsPageable pins the half of #282 the
// first cut missed. A divergence landing on a worker-chunk BOUNDARY
// (rather than inside a chunk) aborted the run without ever touching
// obs.VerifyArchiveMismatchesTotal — so the P1
// stellarindex_stellar_archive_divergence page, which selects that
// counter, could not fire for it. It surfaced only as the
// severity-TICKET stellarindex_verify_archive_unit_failed, which is
// exactly the "a genuine archive-correctness event pages nobody"
// defect #282 exists to remove. With the r1 units' 12 workers there
// are ~11 such boundaries in every nightly run.
//
// Asserts the whole loop the page depends on rather than the
// increment alone: the count must survive
// collectVerifyArchiveMismatches' fold over chunk_idx and land in
// the exported .prom file at the corrected value, under the `reason`
// the runbook documents.
//
// Not t.Parallel: obs.VerifyArchiveMismatchesTotal is process-global.
func TestStitchChunks_BoundaryBreakIsPageable(t *testing.T) {
	tests := []struct {
		name       string
		results    []chunkResult
		wantReason string
	}{
		{
			name: "boundary chain break",
			results: []chunkResult{
				{Idx: 0, FirstSeq: 100, LastSeq: 199, LastHash: hashFrom(0xAA), Verified: 100},
				{Idx: 1, FirstSeq: 200, FirstPrevHash: hashFrom(0xDD), LastSeq: 299, LastHash: hashFrom(0xBB), Verified: 100},
			},
			wantReason: "chain",
		},
		{
			name: "boundary sequence gap",
			results: []chunkResult{
				{Idx: 0, FirstSeq: 100, LastSeq: 199, LastHash: hashFrom(0xAA), Verified: 100},
				{Idx: 1, FirstSeq: 250, FirstPrevHash: hashFrom(0xAA), LastSeq: 299, LastHash: hashFrom(0xBB), Verified: 50},
			},
			wantReason: "sequence",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			obs.VerifyArchiveMismatchesTotal.Reset()
			t.Cleanup(obs.VerifyArchiveMismatchesTotal.Reset)

			if err := stitchChunks(tc.results); err == nil {
				t.Fatal("expected a boundary error; got nil")
			}

			totals := collectVerifyArchiveMismatches()
			if totals[tc.wantReason] != 1 {
				t.Fatalf("collected totals = %v, want %q=1 — the boundary break never reached "+
					"stellarindex_verify_archive_mismatches_total, so the P1 "+
					"stellarindex_stellar_archive_divergence page cannot fire for it (#282)",
					totals, tc.wantReason)
			}

			// …and out through the export path the units use.
			path := filepath.Join(t.TempDir(), "verify_archive_tier_a.prom")
			if err := writeVerifyArchiveTextfile(path, "chain", totals); err != nil {
				t.Fatalf("write textfile: %v", err)
			}
			for _, reason := range verifyArchiveMismatchReasons {
				want := "0"
				if reason == tc.wantReason {
					want = "1"
				}
				assertSample(t, path,
					`stellarindex_verify_archive_mismatches_total{tier="chain",reason="`+reason+`"}`, want)
			}
		})
	}
}

// TestCheckResumeFromHash_MismatchIsPageable — the cross-RUN seam is
// the same correctness class as the cross-chunk one (our bytes do not
// chain onto what the previous run certified), so it feeds the same
// counter. The second half is the load-bearing half: an operator
// typo in -resume-from-hash is an INPUT error, not a divergence, and
// must not move a severity-page counter.
//
// Not t.Parallel: obs.VerifyArchiveMismatchesTotal is process-global.
func TestCheckResumeFromHash_MismatchIsPageable(t *testing.T) {
	obs.VerifyArchiveMismatchesTotal.Reset()
	t.Cleanup(obs.VerifyArchiveMismatchesTotal.Reset)

	if err := checkResumeFromHash("bb"+strings.Repeat("00", 31), hashFrom(0xAA), 100); err == nil {
		t.Fatal("expected mismatch error")
	}
	if got := collectVerifyArchiveMismatches(); got["chain"] != 1 {
		t.Fatalf("collected totals = %v, want chain=1 — a cross-run boundary mismatch never "+
			"reached the P1 counter (#282)", got)
	}

	obs.VerifyArchiveMismatchesTotal.Reset()
	if err := checkResumeFromHash("zzznotahex", hashFrom(0xAA), 100); err == nil {
		t.Fatal("expected a parse error for a malformed -resume-from-hash")
	}
	if got := collectVerifyArchiveMismatches(); len(got) != 0 {
		t.Fatalf("a malformed -resume-from-hash incremented the P1 counter (%v) — "+
			"operator input errors must not page", got)
	}
}
