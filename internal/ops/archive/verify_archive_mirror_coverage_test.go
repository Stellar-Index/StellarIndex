package archive

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	sdkxdr "github.com/stellar/go-stellar-sdk/xdr"
)

// F144. The cross-anchor mirror is filled by its own periodic job, so
// the newest checkpoints the LCM walk reaches have no mirror file yet
// and never did. Measured on r1 2026-09-19: the mirror holds
// 1,007,807 of the 1,007,807 checkpoint files between ledger 63 and
// its high-water 64,499,647 — no hole anywhere — while the nightly
// tier-B run walked to 64,501,171 and logged `matched=325 missed=23`.
// Every one of those 23 was a checkpoint ABOVE 64,499,647; the run
// then advanced the checkpoint tier's high-water to 64,501,171,
// certifying a span the anchor had never been asked about.
//
// These tests pin both halves: an absence beyond the mirror's span is
// not a miss, and the high-water stops at what was anchored.

// mirrorFixture builds a history-archive mirror under a temp root
// holding one gzipped XDR ledger-header file per checkpoint given,
// each carrying the deterministic hash anchorHashFor returns.
func mirrorFixture(t *testing.T, checkpoints ...uint32) string {
	t.Helper()
	root := t.TempDir()
	for _, seq := range checkpoints {
		writeMirrorCheckpoint(t, root, seq)
	}
	return root
}

// anchorHashFor is the canonical hash the fixture mirror records for a
// checkpoint — derived from the sequence so a test can assert the
// matched case without carrying a literal.
func anchorHashFor(seq uint32) sdkxdr.Hash {
	return sha256.Sum256([]byte(fmt.Sprintf("stellar-index test checkpoint %d", seq)))
}

// writeMirrorCheckpoint writes <root>/ledger/XX/YY/ZZ/ledger-<hex>.xdr.gz
// in the archive's own format: a gzipped stream of record-marked
// LedgerHeaderHistoryEntry values. Two entries, so the reader's scan
// for the matching sequence is exercised rather than short-circuited.
func writeMirrorCheckpoint(t *testing.T, root string, seq uint32) {
	t.Helper()
	hexSeq := fmt.Sprintf("%08x", seq)
	dir := filepath.Join(root, "ledger", hexSeq[0:2], hexSeq[2:4], hexSeq[4:6])
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	f, err := os.Create(filepath.Join(dir, "ledger-"+hexSeq+".xdr.gz")) //nolint:gosec // test-only temp path
	if err != nil {
		t.Fatalf("create checkpoint file: %v", err)
	}
	defer func() { _ = f.Close() }()

	gz := gzip.NewWriter(f)
	for _, s := range []uint32{seq - 1, seq} {
		entry := sdkxdr.LedgerHeaderHistoryEntry{
			Hash:   anchorHashFor(s),
			Header: sdkxdr.LedgerHeader{LedgerSeq: sdkxdr.Uint32(s)},
		}
		var body bytes.Buffer
		if _, err := sdkxdr.Marshal(&body, entry); err != nil {
			t.Fatalf("marshal entry %d: %v", s, err)
		}
		// RFC 4506 record marking: length with the last-fragment bit.
		if err := binary.Write(gz, binary.BigEndian, uint32(body.Len())|0x80000000); err != nil {
			t.Fatalf("write frame header: %v", err)
		}
		if _, err := gz.Write(body.Bytes()); err != nil {
			t.Fatalf("write entry %d: %v", s, err)
		}
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
}

func TestClassifyCheckpointAnchor_AbsenceBeyondTheMirrorIsNotAMiss(t *testing.T) {
	t.Parallel()
	// The mirror holds 127 and 191; 255 is the trailing checkpoint the
	// walk reaches before the fill job does.
	root := mirrorFixture(t, 127, 191)
	cov := readArchiveMirrorCoverage(root)
	if !cov.Known || cov.Floor != 127 || cov.HighWater != 191 {
		t.Fatalf("mirror coverage = %+v, want floor 127 high-water 191 known", cov)
	}

	for _, tc := range []struct {
		name string
		seq  uint32
		our  sdkxdr.Hash
		want checkpointAnchorOutcome
	}{
		{"in-coverage and equal", 191, anchorHashFor(191), checkpointAnchorMatched},
		{"above the high-water", 255, anchorHashFor(255), checkpointAnchorUnmirrored},
		{"below the floor", 63, anchorHashFor(63), checkpointAnchorUnmirrored},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := classifyCheckpointAnchor(root, tc.seq, tc.our, cov)
			if err != nil {
				t.Fatalf("classifyCheckpointAnchor(%d): %v", tc.seq, err)
			}
			if got != tc.want {
				t.Errorf("checkpoint %d classified %q, want %q — a checkpoint outside the "+
					"mirror's span [%d, %d] is a fill lag, not a hole in the cross-anchor "+
					"archive (F144)", tc.seq, got, tc.want, cov.Floor, cov.HighWater)
			}
		})
	}
}

func TestClassifyCheckpointAnchor_HoleInsideTheMirrorIsStillAMiss(t *testing.T) {
	t.Parallel()
	// 191 is absent from a mirror that holds both 127 and 255, so it
	// is a genuine hole — exactly what ADR-0017 contract 3 forbids and
	// what -fail-on-missed must keep catching.
	root := mirrorFixture(t, 127, 255)
	cov := readArchiveMirrorCoverage(root)
	got, err := classifyCheckpointAnchor(root, 191, anchorHashFor(191), cov)
	if err != nil {
		t.Fatalf("classifyCheckpointAnchor(191): %v", err)
	}
	if got != checkpointAnchorMissed {
		t.Errorf("checkpoint 191 classified %q, want %q — it is absent from inside the "+
			"mirror's own span [%d, %d]", got, checkpointAnchorMissed, cov.Floor, cov.HighWater)
	}
}

func TestClassifyCheckpointAnchor_DivergenceStillAborts(t *testing.T) {
	t.Parallel()
	root := mirrorFixture(t, 127)
	cov := readArchiveMirrorCoverage(root)
	ours := sha256.Sum256([]byte("a hash the archive did not sign"))
	if _, err := classifyCheckpointAnchor(root, 127, ours, cov); err == nil {
		t.Fatal("classifyCheckpointAnchor returned nil for a hash that disagrees with the " +
			"archive-signed one — a cross-anchor divergence must abort the walk")
	}
}

func TestClassifyCheckpointAnchor_UnknownCoverageToleratesNothing(t *testing.T) {
	t.Parallel()
	// An unreadable -archive-root must not read as "everything is
	// outside coverage": with no measurable span every absence stays a
	// miss, which is the behaviour before the span existed.
	root := filepath.Join(t.TempDir(), "no-such-mirror")
	cov := readArchiveMirrorCoverage(root)
	if cov.Known {
		t.Fatalf("coverage of a missing mirror = %+v, want unknown", cov)
	}
	got, err := classifyCheckpointAnchor(root, 4095, anchorHashFor(4095), cov)
	if err != nil {
		t.Fatalf("classifyCheckpointAnchor: %v", err)
	}
	if got != checkpointAnchorMissed {
		t.Errorf("classified %q against an unmeasurable mirror, want %q", got, checkpointAnchorMissed)
	}
}

func TestReadArchiveMirrorCoverage_SkipsNonCheckpointLeaves(t *testing.T) {
	t.Parallel()
	root := mirrorFixture(t, 127, 191)
	// An archivist scratch file and an empty branch must not move the
	// measured bounds off a checkpoint boundary.
	scratch := filepath.Join(root, "ledger", "00", "00", "00", "ledger-0000007f.xdr.gz.tmp")
	if err := os.WriteFile(scratch, []byte("partial"), 0o600); err != nil {
		t.Fatalf("write scratch file: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "ledger", "ff", "ff", "ff"), 0o755); err != nil {
		t.Fatalf("mkdir empty branch: %v", err)
	}
	cov := readArchiveMirrorCoverage(root)
	if !cov.Known || cov.Floor != 127 || cov.HighWater != 191 {
		t.Errorf("mirror coverage = %+v, want floor 127 high-water 191 known", cov)
	}
}

func TestCheckpointWatermark_StopsAtWhatTheMirrorCouldAnchor(t *testing.T) {
	t.Parallel()
	// The r1 numbers of 2026-09-19: the walk reached 64,501,171 and
	// the mirror held nothing above 64,499,647.
	const (
		walkReached = uint32(64501171)
		mirrorHigh  = uint32(64499647)
	)
	cov := archiveMirrorCoverage{Floor: 63, HighWater: mirrorHigh, Known: true}
	if got := checkpointWatermark(walkReached, cov); got != mirrorHigh {
		t.Errorf("checkpointWatermark = %d, want %d — the 23 checkpoints above the mirror's "+
			"high-water were never anchored, so the tier must not certify them (F144)", got, mirrorHigh)
	}
	// A walk that stays inside the span certifies everything it walked.
	if got := checkpointWatermark(mirrorHigh-6400, cov); got != mirrorHigh-6400 {
		t.Errorf("checkpointWatermark = %d, want %d — a walk inside the mirror's span is fully anchored",
			got, mirrorHigh-6400)
	}
	// Unknown coverage keeps the pre-existing behaviour.
	if got := checkpointWatermark(walkReached, archiveMirrorCoverage{}); got != walkReached {
		t.Errorf("checkpointWatermark with unknown coverage = %d, want %d", got, walkReached)
	}
}

func TestApplyCheckpointTierState_DoesNotCertifyAnUnanchoredSpan(t *testing.T) {
	t.Parallel()
	const (
		walkReached = uint32(64501171)
		mirrorHigh  = uint32(64499647)
	)
	prior := VerifyArchiveState{Tiers: map[string]VerifyArchiveTierState{
		"checkpoint": {LastVerifiedLedger: 64494000},
	}}
	cov := archiveMirrorCoverage{Floor: 63, HighWater: mirrorHigh, Known: true}

	next, anchored := applyCheckpointTierState(prior, walkReached, cov, time.Now().UTC())
	if anchored != mirrorHigh {
		t.Errorf("anchored ledger = %d, want %d", anchored, mirrorHigh)
	}
	if got := next.Tiers["checkpoint"].LastVerifiedLedger; got != mirrorHigh {
		t.Errorf("checkpoint high-water = %d, want %d — advancing to the walk's tip records the "+
			"unanchored trailing span as cross-anchor-verified, and -from-last-verified then "+
			"starts the next run above it (F144)", got, mirrorHigh)
	}
	if got := next.Tiers["chain"].LastVerifiedLedger; got != 0 {
		t.Errorf("chain high-water = %d, want 0 — the clamp is the checkpoint tier's alone", got)
	}
}

func TestCheckpointAnchorReached_NothingAnchoredIsInconclusive(t *testing.T) {
	t.Parallel()
	// DAT-09 under the coverage taxonomy: a walk that ran entirely
	// above the mirror's high-water matched nothing and missed
	// nothing, and must not pass as a clean anchor run.
	if err := checkpointAnchorReached(0, 0, 12); err == nil {
		t.Error("checkpointAnchorReached(0 matched, 0 missed, 12 unmirrored) = nil — a run that " +
			"anchored nothing must not be certified")
	}
	if err := checkpointAnchorReached(0, 12, 0); err == nil {
		t.Error("checkpointAnchorReached(0 matched, 12 missed, 0 unmirrored) = nil — DAT-09")
	}
	// The r1 steady state: matches inside the span, the rest beyond it.
	if err := checkpointAnchorReached(325, 0, 23); err != nil {
		t.Errorf("checkpointAnchorReached(325, 0, 23) = %v, want nil — the measured tier-B shape", err)
	}
	// A checkpoint-free range (short walk between checkpoints).
	if err := checkpointAnchorReached(0, 0, 0); err != nil {
		t.Errorf("checkpointAnchorReached(0, 0, 0) = %v, want nil", err)
	}
}
