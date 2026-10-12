package archivecompleteness_test

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/archivecompleteness"
)

// TestWriteTextfile_BasicShape — happy path: a populated snapshot
// produces well-formed Prometheus text exposition with the expected
// HELP/TYPE prefix and metric lines.
func TestWriteTextfile_BasicShape(t *testing.T) {
	snap := archivecompleteness.NewMetricsSnapshot()
	snap.FilesMissing["cross-anchor"] = 0
	snap.FilesMissing["galexie-archive"] = 12
	snap.RunDurationSeconds = 73.421
	snap.LastSuccessTimestamp = time.Date(2026, 4, 28, 10, 0, 0, 0, time.UTC)
	snap.RepairAttempts["sdf-core-live-001"] = 3
	snap.RepairFailures["multi-source-exhausted"] = 1

	var buf bytes.Buffer
	if err := archivecompleteness.WriteTextfile(&buf, snap); err != nil {
		t.Fatalf("WriteTextfile: %v", err)
	}
	out := buf.String()

	expectedSubstrings := []string{
		"# HELP archive_files_missing",
		"# TYPE archive_files_missing gauge",
		`archive_files_missing{archive="cross-anchor"} 0`,
		`archive_files_missing{archive="galexie-archive"} 12`,
		"# TYPE archive_completeness_last_success_timestamp gauge",
		"archive_completeness_last_success_timestamp 1777370400",
		"# TYPE archive_completeness_run_duration_seconds gauge",
		"archive_completeness_run_duration_seconds 73.421",
		"# TYPE archive_completeness_repair_attempts_total counter",
		`archive_completeness_repair_attempts_total{source="sdf-core-live-001"} 3`,
		"# TYPE archive_completeness_repair_failures_total counter",
		`archive_completeness_repair_failures_total{source="multi-source-exhausted"} 1`,
	}
	for _, want := range expectedSubstrings {
		if !strings.Contains(out, want) {
			t.Errorf("textfile output missing %q\n--- got ---\n%s", want, out)
		}
	}
}

// TestWriteTextfile_ChecksExpectedFoundDenominator pins that PopulateFromReport carries Expected/Found onto the
// textfile, since a zero archive_files_missing is indistinguishable from a vacuous range without a denominator; alerts
// can require checkpoints_expected > 0.
func TestWriteTextfile_ChecksExpectedFoundDenominator(t *testing.T) {
	snap := archivecompleteness.NewMetricsSnapshot()
	report := archivecompleteness.NewReport(100, 200)
	report.SetCrossAnchor("/mnt/archive", archivecompleteness.CrossAnchorResult{
		Expected: 1024,
		Found:    1024,
	})
	snap.PopulateFromReport(report)

	var buf bytes.Buffer
	if err := archivecompleteness.WriteTextfile(&buf, snap); err != nil {
		t.Fatalf("WriteTextfile: %v", err)
	}
	out := buf.String()

	expectedSubstrings := []string{
		"# TYPE archive_checkpoints_expected gauge",
		`archive_checkpoints_expected{archive="cross-anchor"} 1024`,
		"# TYPE archive_checkpoints_found gauge",
		`archive_checkpoints_found{archive="cross-anchor"} 1024`,
	}
	for _, want := range expectedSubstrings {
		if !strings.Contains(out, want) {
			t.Errorf("textfile output missing %q\n--- got ---\n%s", want, out)
		}
	}
}

// TestWriteTextfile_EmptySnapshotProducesOnlyHelp pins that a snapshot with no missing files or repair activity still
// emits well-formed output (just help/type for files_missing, no samples); empty maps are valid input.
func TestWriteTextfile_EmptySnapshotProducesOnlyHelp(t *testing.T) {
	snap := archivecompleteness.NewMetricsSnapshot()
	var buf bytes.Buffer
	if err := archivecompleteness.WriteTextfile(&buf, snap); err != nil {
		t.Fatalf("WriteTextfile: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "archive_files_missing gauge") {
		t.Errorf("missing files_missing TYPE line:\n%s", out)
	}
	// No sample lines for files_missing because the map is empty.
	if strings.Contains(out, "archive_files_missing{") {
		t.Errorf("unexpected sample line in empty snapshot:\n%s", out)
	}
}

// TestWriteTextfile_NilSnapshotIsNoop — defensive guard against
// callers passing nil. Should produce no output and no error.
func TestWriteTextfile_NilSnapshotIsNoop(t *testing.T) {
	var buf bytes.Buffer
	if err := archivecompleteness.WriteTextfile(&buf, nil); err != nil {
		t.Errorf("nil snapshot should be a no-op, got error: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("nil snapshot wrote %d bytes, want 0", buf.Len())
	}
}

// TestPopulateFromReport — convenience wrapper preserves the
// per-archive missing-counts on the snapshot.
func TestPopulateFromReport(t *testing.T) {
	r := archivecompleteness.NewReport(2, 1000)
	r.SetCrossAnchor("/srv/history-archive", archivecompleteness.CrossAnchorResult{
		Expected: 10, Found: 7, Missing: []uint32{63, 127, 191},
	})

	snap := archivecompleteness.NewMetricsSnapshot()
	snap.PopulateFromReport(r)

	if snap.FilesMissing["cross-anchor"] != 3 {
		t.Errorf("cross-anchor missing = %d, want 3", snap.FilesMissing["cross-anchor"])
	}
	if _, present := snap.FilesMissing["galexie-archive"]; present {
		t.Errorf("galexie-archive should not be in snapshot when Report.Primary is nil")
	}
}

// TestPopulateFromFillResult pins that repair attempts and failures are populated under REAL source names: a synthetic
// `multi-source-exhausted` failure label would never intersect the attempts labels, leaving the
// `archive-completeness.yml` failure/attempt alert a zero numerator. One attempt per try, one failure per failed try.
func TestPopulateFromFillResult(t *testing.T) {
	res := archivecompleteness.FillResult{
		Filled:           5,
		PerSourceSuccess: map[string]int{"sdf-core-live-001": 4, "lobstr-v1": 1},
		PerSourceAttempt: map[string]int{"sdf-core-live-001": 4, "lobstr-v1": 3, "publicnode-bootes": 2},
		PerSourceFailure: map[string]int{"lobstr-v1": 2, "publicnode-bootes": 2},
		Failed:           []archivecompleteness.FillFailure{{Seq: 63, Reason: "exhausted"}},
	}
	snap := archivecompleteness.NewMetricsSnapshot()
	snap.PopulateFromFillResult(res)

	wantAttempts := map[string]int{
		"sdf-core-live-001": 4,
		"lobstr-v1":         3,
		"publicnode-bootes": 2,
	}
	for source, want := range wantAttempts {
		if got := snap.RepairAttempts[source]; got != want {
			t.Errorf("RepairAttempts[%q] = %d, want %d", source, got, want)
		}
	}
	wantFailures := map[string]int{
		"lobstr-v1":         2,
		"publicnode-bootes": 2,
	}
	for source, want := range wantFailures {
		if got := snap.RepairFailures[source]; got != want {
			t.Errorf("RepairFailures[%q] = %d, want %d", source, got, want)
		}
	}
	// The synthetic label must be gone entirely — an alert grouping
	// `by (source)` would otherwise carry a phantom series that
	// matches no attempt denominator.
	if _, present := snap.RepairFailures["multi-source-exhausted"]; present {
		t.Errorf("synthetic 'multi-source-exhausted' failure label still emitted: %v", snap.RepairFailures)
	}
	// Every failure label must have an attempt label to divide by.
	for source := range snap.RepairFailures {
		if snap.RepairAttempts[source] == 0 {
			t.Errorf("failure label %q has no attempts denominator — by(source) ratio is unfireable", source)
		}
	}
}

// TestWriteTextfile_StableOrdering pins that repeated runs of one snapshot give byte-identical output, which ops rely
// on because a node_exporter scrape compares successive textfiles by content to detect no change.
func TestWriteTextfile_StableOrdering(t *testing.T) {
	snap := archivecompleteness.NewMetricsSnapshot()
	snap.FilesMissing["cross-anchor"] = 0
	snap.FilesMissing["galexie-archive"] = 0
	snap.RepairAttempts["lobstr-v1"] = 1
	snap.RepairAttempts["sdf-core-live-001"] = 2
	snap.RepairAttempts["sdf-core-live-002"] = 1

	var first, second bytes.Buffer
	_ = archivecompleteness.WriteTextfile(&first, snap)
	_ = archivecompleteness.WriteTextfile(&second, snap)

	if first.String() != second.String() {
		t.Errorf("output not stable across writes:\n--- first ---\n%s\n--- second ---\n%s", first.String(), second.String())
	}
}

// A host that has NEVER had a clean run must publish 0, not nothing: `time() - 0` is enormous so the staleness alert
// fires, which is correct, whereas an absent series would make a never-verified archive look healthy.
func TestWriteTextfile_NeverSucceededEmitsZeroNotAbsence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive_completeness.prom")
	snap := archivecompleteness.NewMetricsSnapshot()
	snap.FilesMissing["cross-anchor"] = 3
	if err := archivecompleteness.WriteTextfileAtomic(path, snap); err != nil {
		t.Fatalf("WriteTextfileAtomic: %v", err)
	}
	if got := sampleValue(t, readTextfile(t, path), "archive_completeness_last_success_timestamp"); got != 0 {
		t.Errorf("never-succeeded last_success = %d, want 0", got)
	}
}
