package archivecompleteness_test

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/archivecompleteness"
)

// makeTestArchive builds a minimal `<root>/ledger/XX/YY/ZZ/ledger-…xdr.gz`
// tree with checkpoint files at the supplied sequences. Each file
// contains a valid, non-empty gzip stream — Check() runs a cheap
// structural probe (non-empty on disk + valid gzip + non-empty
// decompressed content, DAT-09/DAT-11) on every present file, so a
// "present" fixture must pass that probe to be counted as Found.
func makeTestArchive(t *testing.T, present []uint32) string {
	t.Helper()
	root := t.TempDir()
	for _, seq := range present {
		writeCheckpointFile(t, root, seq, gzipBytes(t, "x"))
	}
	return root
}

// gzipBytes gzip-compresses s. Shared by the structural-probe tests
// below to build both valid and deliberately-corrupt fixtures.
func gzipBytes(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write([]byte(s)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// writeCheckpointFile writes raw bytes `content` at the canonical
// checkpoint path for seq under root, creating parent directories as
// needed. Used both for valid fixtures (gzipBytes output) and
// deliberately-corrupt ones (empty, truncated, non-gzip) to exercise
// the structural-probe rejection paths.
func writeCheckpointFile(t *testing.T, root string, seq uint32, content []byte) string {
	t.Helper()
	hex := fmt.Sprintf("%08x", seq)
	dir := filepath.Join(root, "ledger", hex[0:2], hex[2:4], hex[4:6])
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	path := filepath.Join(dir, "ledger-"+hex+".xdr.gz")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// TestCheck_AllPresent — fully-populated archive across the requested
// range produces missing-count 0.
func TestCheck_AllPresent(t *testing.T) {
	// Range [0, 191] covers checkpoints at 63, 127, 191.
	root := makeTestArchive(t, []uint32{63, 127, 191})
	c := archivecompleteness.NewCrossAnchorChecker(root)

	res, err := c.Check(0, 191)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Expected != 3 {
		t.Errorf("Expected = %d, want 3", res.Expected)
	}
	if res.Found != 3 {
		t.Errorf("Found = %d, want 3", res.Found)
	}
	if len(res.Missing) != 0 {
		t.Errorf("Missing = %v, want empty", res.Missing)
	}
}

// TestCheck_AllMissing — empty archive across a 3-checkpoint range.
func TestCheck_AllMissing(t *testing.T) {
	root := makeTestArchive(t, nil) // archive root exists but no checkpoint files
	c := archivecompleteness.NewCrossAnchorChecker(root)

	res, err := c.Check(0, 191)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Expected != 3 {
		t.Errorf("Expected = %d, want 3", res.Expected)
	}
	if res.Found != 0 {
		t.Errorf("Found = %d, want 0", res.Found)
	}
	if len(res.Missing) != 3 {
		t.Errorf("Missing = %v, want 3 entries", res.Missing)
	}
	wantSeqs := []uint32{63, 127, 191}
	for i, want := range wantSeqs {
		if res.Missing[i] != want {
			t.Errorf("Missing[%d] = %d, want %d", i, res.Missing[i], want)
		}
	}
}

// TestCheck_PartialMissing — some present, some missing. Verifies
// the Missing list contains exactly the absent positions and the
// counts match.
func TestCheck_PartialMissing(t *testing.T) {
	// Present: 63 and 191. Missing: 127.
	root := makeTestArchive(t, []uint32{63, 191})
	c := archivecompleteness.NewCrossAnchorChecker(root)

	res, err := c.Check(0, 191)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Expected != 3 || res.Found != 2 {
		t.Errorf("counts: Expected=%d Found=%d, want 3 / 2", res.Expected, res.Found)
	}
	if len(res.Missing) != 1 || res.Missing[0] != 127 {
		t.Errorf("Missing = %v, want [127]", res.Missing)
	}
}

// TestCheck_RangeAlignment — non-checkpoint-aligned `from` / `to`
// values still produce the correct Expected count from the enclosed
// checkpoints.
func TestCheck_RangeAlignment(t *testing.T) {
	// Range [50, 200] should enclose checkpoints at 63, 127, 191
	// (191 is the last checkpoint <= 200).
	root := makeTestArchive(t, []uint32{63, 127, 191})
	c := archivecompleteness.NewCrossAnchorChecker(root)

	res, err := c.Check(50, 200)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Expected != 3 {
		t.Errorf("Expected = %d, want 3", res.Expected)
	}
	if res.Found != 3 {
		t.Errorf("Found = %d, want 3", res.Found)
	}
}

// TestCheck_RangeBelowFirstCheckpoint — a range entirely below
// seq=63 contains no checkpoint. Empty result, no error.
func TestCheck_RangeBelowFirstCheckpoint(t *testing.T) {
	root := makeTestArchive(t, nil)
	c := archivecompleteness.NewCrossAnchorChecker(root)

	res, err := c.Check(0, 50)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Expected != 0 || res.Found != 0 || len(res.Missing) != 0 {
		t.Errorf("range below first checkpoint should be empty, got %+v", res)
	}
}

// TestCheck_ZeroByteFileIsMissing is the DAT-09/DAT-11 regression: a
// present-but-zero-length checkpoint file must be counted as
// Missing, not Found — a zero-byte file proves nothing was ever
// actually written there.
func TestCheck_ZeroByteFileIsMissing(t *testing.T) {
	root := makeTestArchive(t, []uint32{63, 191})
	writeCheckpointFile(t, root, 127, nil) // present but empty

	c := archivecompleteness.NewCrossAnchorChecker(root)
	res, err := c.Check(0, 191)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Expected != 3 || res.Found != 2 {
		t.Fatalf("counts: Expected=%d Found=%d, want 3 / 2", res.Expected, res.Found)
	}
	if len(res.Missing) != 1 || res.Missing[0] != 127 {
		t.Fatalf("Missing = %v, want [127] (the zero-byte file)", res.Missing)
	}
}

// TestCheck_CorruptGzipIsMissing: a present file that is NOT valid
// gzip (truncated download, HTML error page saved verbatim) must be
// counted as Missing so the fill path re-fetches it.
func TestCheck_CorruptGzipIsMissing(t *testing.T) {
	root := makeTestArchive(t, []uint32{63})
	writeCheckpointFile(t, root, 127, []byte("<html>502 Bad Gateway</html>"))

	c := archivecompleteness.NewCrossAnchorChecker(root)
	res, err := c.Check(0, 191)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Found != 1 {
		t.Errorf("Found = %d, want 1 (only the valid file)", res.Found)
	}
	if len(res.Missing) != 2 { // 127 (corrupt) + 191 (never written)
		t.Fatalf("Missing = %v, want 2 entries (corrupt 127 + absent 191)", res.Missing)
	}
	found127 := false
	for _, m := range res.Missing {
		if m == 127 {
			found127 = true
		}
	}
	if !found127 {
		t.Errorf("Missing should include the corrupt file's seq 127; got %v", res.Missing)
	}
}

// TestCheck_ZeroDecompressedContentIsMissing: a technically-valid
// gzip stream that decompresses to ZERO bytes (an empty payload
// gzip'd) must also be treated as missing — DAT-09's "require
// decompressed size > 0".
func TestCheck_ZeroDecompressedContentIsMissing(t *testing.T) {
	root := makeTestArchive(t, []uint32{63})
	writeCheckpointFile(t, root, 127, gzipBytes(t, "")) // valid gzip, empty payload

	c := archivecompleteness.NewCrossAnchorChecker(root)
	res, err := c.Check(0, 191)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Found != 1 {
		t.Errorf("Found = %d, want 1", res.Found)
	}
	found127 := false
	for _, m := range res.Missing {
		if m == 127 {
			found127 = true
		}
	}
	if !found127 {
		t.Errorf("Missing should include seq 127 (zero decompressed content); got %v", res.Missing)
	}
}

// TestCheck_InvalidArchiveRoot — non-existent path returns an error.
func TestCheck_InvalidArchiveRoot(t *testing.T) {
	c := archivecompleteness.NewCrossAnchorChecker("/nonexistent/path/that/does/not/exist")
	_, err := c.Check(0, 191)
	if err == nil {
		t.Fatal("expected error for missing archiveRoot, got nil")
	}
}

// TestCheck_EmptyArchiveRoot — explicit empty string is rejected.
func TestCheck_EmptyArchiveRoot(t *testing.T) {
	c := archivecompleteness.NewCrossAnchorChecker("")
	_, err := c.Check(0, 191)
	if err == nil {
		t.Fatal("expected error for empty archiveRoot, got nil")
	}
}

// TestCheck_ToLessThanFrom — invalid range is an error.
func TestCheck_ToLessThanFrom(t *testing.T) {
	root := makeTestArchive(t, nil)
	c := archivecompleteness.NewCrossAnchorChecker(root)
	_, err := c.Check(200, 100)
	if err == nil {
		t.Fatal("expected error for to < from, got nil")
	}
}

// TestCheck_ArchiveRootIsFile — operator passes a regular file path
// instead of a directory; surface a clear error rather than walking
// nonsense.
func TestCheck_ArchiveRootIsFile(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(tmp, []byte("x"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	c := archivecompleteness.NewCrossAnchorChecker(tmp)
	_, err := c.Check(0, 191)
	if err == nil {
		t.Fatal("expected error for archiveRoot=file, got nil")
	}
}

// TestReport_RoundTripJSON — Report → JSON → struct should
// preserve every field. Wire shape lock for downstream tools.
func TestReport_RoundTripJSON(t *testing.T) {
	r := archivecompleteness.NewReport(2, 191)
	r.SetCrossAnchor("/tmp/test-archive", archivecompleteness.CrossAnchorResult{
		From:     2,
		To:       191,
		Expected: 3,
		Found:    2,
		Missing:  []uint32{127},
	})

	var buf bytes.Buffer
	if err := r.WriteJSON(&buf); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}

	var got archivecompleteness.Report
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Schema != "1" {
		t.Errorf("Schema = %q, want \"1\"", got.Schema)
	}
	if got.Range.From != 2 || got.Range.To != 191 {
		t.Errorf("Range = %+v", got.Range)
	}
	if got.CrossAnchor == nil {
		t.Fatal("CrossAnchor section missing")
	}
	if got.CrossAnchor.ArchiveRoot != "/tmp/test-archive" {
		t.Errorf("ArchiveRoot = %q", got.CrossAnchor.ArchiveRoot)
	}
	if got.CrossAnchor.MissingCount != 1 {
		t.Errorf("MissingCount = %d, want 1", got.CrossAnchor.MissingCount)
	}
	if len(got.CrossAnchor.Missing) != 1 || got.CrossAnchor.Missing[0] != 127 {
		t.Errorf("Missing = %v, want [127]", got.CrossAnchor.Missing)
	}
}

// TestReport_AnyMissing — convenience predicate fires when either
// section has gaps; clean when all sections clean or absent.
func TestReport_AnyMissing(t *testing.T) {
	r := archivecompleteness.NewReport(2, 191)
	if r.AnyMissing() {
		t.Error("empty report should not report missing")
	}
	r.SetCrossAnchor("/r", archivecompleteness.CrossAnchorResult{Expected: 3, Found: 3})
	if r.AnyMissing() {
		t.Error("clean cross-anchor: should not report missing")
	}
	r.SetCrossAnchor("/r", archivecompleteness.CrossAnchorResult{Expected: 3, Found: 2, Missing: []uint32{127}})
	if !r.AnyMissing() {
		t.Error("cross-anchor with gap: should report missing")
	}
}

// TestReport_Vacuous is the DAT-11 regression: a report whose only
// populated section scanned ZERO expected checkpoint positions
// (Expected=0) must be flagged Vacuous — "clean" per AnyMissing() but
// NOTHING was actually verified. A section that scanned something
// (Expected > 0, even if clean) is NOT vacuous.
func TestReport_Vacuous(t *testing.T) {
	t.Run("no section populated is not vacuous", func(t *testing.T) {
		r := archivecompleteness.NewReport(2, 191)
		if r.Vacuous() {
			t.Error("an empty (no-section) report should not be flagged Vacuous")
		}
	})
	t.Run("Expected=0 cross-anchor section is vacuous", func(t *testing.T) {
		r := archivecompleteness.NewReport(2, 50) // range with no checkpoint position
		r.SetCrossAnchor("/r", archivecompleteness.CrossAnchorResult{Expected: 0, Found: 0})
		if !r.Vacuous() {
			t.Error("Expected=0 cross-anchor section should be Vacuous")
		}
		if r.AnyMissing() {
			t.Error("sanity: Expected=0/Found=0 must NOT trip AnyMissing (that's the bug this guards against)")
		}
	})
	t.Run("Expected>0 clean section is not vacuous", func(t *testing.T) {
		r := archivecompleteness.NewReport(2, 191)
		r.SetCrossAnchor("/r", archivecompleteness.CrossAnchorResult{Expected: 3, Found: 3})
		if r.Vacuous() {
			t.Error("a section that scanned something (Expected=3) should not be Vacuous")
		}
	})
	t.Run("Expected>0 with missing is not vacuous", func(t *testing.T) {
		r := archivecompleteness.NewReport(2, 191)
		r.SetCrossAnchor("/r", archivecompleteness.CrossAnchorResult{Expected: 3, Found: 2, Missing: []uint32{127}})
		if r.Vacuous() {
			t.Error("a section with real missing files should not be Vacuous (it's AnyMissing instead)")
		}
	})
}

// TestReport_TruncatedFlagSurvivesJSON — when a catastrophic gap
// trips the MaxMissingReported cap, Truncated must round-trip on
// the wire so consumers know the list is partial.
func TestReport_TruncatedFlagSurvivesJSON(t *testing.T) {
	r := archivecompleteness.NewReport(2, 1<<20)
	r.SetCrossAnchor("/r", archivecompleteness.CrossAnchorResult{
		Expected:  100000,
		Found:     0,
		Missing:   []uint32{63, 127, 191}, // partial — pretend the rest were truncated
		Truncated: true,
	})

	var buf bytes.Buffer
	if err := r.WriteJSON(&buf); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	var got archivecompleteness.Report
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !got.CrossAnchor.Truncated {
		t.Error("Truncated didn't survive the round-trip")
	}
}
