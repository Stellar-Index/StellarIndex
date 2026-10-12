package archivecompleteness_test

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
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

// `verify` sets LastSuccessTimestamp only on a clean, non-vacuous run. The writer must not omit last_success when it
// is zero: the series would vanish on scrape and `(time() - archive_completeness_last_success_timestamp) > 26h` would
// yield no samples, silencing the staleness alert exactly when it must fire.

// sampleValue extracts a single unlabelled sample's value from a
// textfile body. Fails the test when the series is absent — which is
// the whole point: absence is the defect.
func sampleValue(t *testing.T, body, metric string) int64 {
	t.Helper()
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(metric) + ` (-?\d+)$`)
	m := re.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("series %q absent from textfile — node_exporter re-reads this file each scrape, so an absent series means the alert has no samples to evaluate\n--- got ---\n%s", metric, body)
	}
	v, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		t.Fatalf("parse %q value %q: %v", metric, m[1], err)
	}
	return v
}

// labelledSampleValue extracts a `metric{source="…"} <int>` sample.
// Returns ok=false when that (metric, source) pair is absent.
func labelledSampleValue(t *testing.T, body, metric, source string) (int64, bool) {
	t.Helper()
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(metric+`{source="`+source+`"}`) + ` (-?\d+)$`)
	m := re.FindStringSubmatch(body)
	if m == nil {
		return 0, false
	}
	v, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		t.Fatalf("parse %q{%s} value %q: %v", metric, source, m[1], err)
	}
	return v, true
}

func readTextfile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // test-controlled temp path
	if err != nil {
		t.Fatalf("read textfile: %v", err)
	}
	return string(b)
}

// TestWriteTextfileAtomic_LastSuccessSurvivesFailedRun pins the core: after a clean run stamps the timestamp, a later
// RESIDUAL run rewrites the file and the last_success series must still be present with the clean run's value.
func TestWriteTextfileAtomic_LastSuccessSurvivesFailedRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive_completeness.prom")
	cleanAt := time.Date(2026, 7, 25, 3, 0, 0, 0, time.UTC)

	// Run 1 — clean.
	clean := archivecompleteness.NewMetricsSnapshot()
	clean.FilesMissing["cross-anchor"] = 0
	clean.RunDurationSeconds = 12.5
	clean.LastSuccessTimestamp = cleanAt
	if err := archivecompleteness.WriteTextfileAtomic(path, clean); err != nil {
		t.Fatalf("WriteTextfileAtomic (clean): %v", err)
	}
	if got := sampleValue(t, readTextfile(t, path), "archive_completeness_last_success_timestamp"); got != cleanAt.Unix() {
		t.Fatalf("after clean run: last_success = %d, want %d", got, cleanAt.Unix())
	}

	// Runs 2 and 3 are residuals with no fresh success timestamp. The series must persist across BOTH rewrites; a
	// carry-forward surviving one generation would still blind the 48h critical-stale alert.
	for run := 2; run <= 3; run++ {
		residual := archivecompleteness.NewMetricsSnapshot()
		residual.FilesMissing["cross-anchor"] = 7
		residual.RunDurationSeconds = 30.25
		if err := archivecompleteness.WriteTextfileAtomic(path, residual); err != nil {
			t.Fatalf("WriteTextfileAtomic (residual run %d): %v", run, err)
		}
		body := readTextfile(t, path)
		if got := sampleValue(t, body, "archive_completeness_last_success_timestamp"); got != cleanAt.Unix() {
			t.Errorf("after residual run %d: last_success = %d, want %d (the last CLEAN run)",
				run, got, cleanAt.Unix())
		}
		// Sanity: the run-scoped series did get replaced.
		if !strings.Contains(body, `archive_files_missing{archive="cross-anchor"} 7`) {
			t.Errorf("after residual run %d: files_missing not refreshed\n%s", run, body)
		}
	}

	// Run 4 — clean again: the timestamp must ADVANCE, not stick.
	laterAt := cleanAt.Add(48 * time.Hour)
	advance := archivecompleteness.NewMetricsSnapshot()
	advance.FilesMissing["cross-anchor"] = 0
	advance.LastSuccessTimestamp = laterAt
	if err := archivecompleteness.WriteTextfileAtomic(path, advance); err != nil {
		t.Fatalf("WriteTextfileAtomic (clean 2): %v", err)
	}
	if got := sampleValue(t, readTextfile(t, path), "archive_completeness_last_success_timestamp"); got != laterAt.Unix() {
		t.Errorf("after second clean run: last_success = %d, want %d (must advance)", got, laterAt.Unix())
	}
}

// The repair counters carry the `_total` suffix and a `# TYPE … counter` declaration, and alert rules apply increase()
// to them, so the on-disk value must accumulate across runs instead of being replaced by each run's fragment.
func TestWriteTextfileAtomic_RepairCountersAccumulate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive_completeness.prom")

	first := archivecompleteness.NewMetricsSnapshot()
	first.PopulateFromFillResult(archivecompleteness.FillResult{
		PerSourceAttempt: map[string]int{"lobstr-v1": 3, "sdf-core-live-001": 1},
		PerSourceFailure: map[string]int{"lobstr-v1": 3},
	})
	if err := archivecompleteness.WriteTextfileAtomic(path, first); err != nil {
		t.Fatalf("WriteTextfileAtomic (1): %v", err)
	}

	second := archivecompleteness.NewMetricsSnapshot()
	second.PopulateFromFillResult(archivecompleteness.FillResult{
		PerSourceAttempt: map[string]int{"lobstr-v1": 2, "sdf-core-live-001": 2},
		PerSourceFailure: map[string]int{"lobstr-v1": 2},
	})
	if err := archivecompleteness.WriteTextfileAtomic(path, second); err != nil {
		t.Fatalf("WriteTextfileAtomic (2): %v", err)
	}

	body := readTextfile(t, path)
	for _, tc := range []struct {
		metric, source string
		want           int64
	}{
		{"archive_completeness_repair_attempts_total", "lobstr-v1", 5},
		{"archive_completeness_repair_attempts_total", "sdf-core-live-001", 3},
		{"archive_completeness_repair_failures_total", "lobstr-v1", 5},
	} {
		got, ok := labelledSampleValue(t, body, tc.metric, tc.source)
		if !ok {
			t.Errorf("%s{source=%q} absent — a counter that vanishes cannot be rate()d\n%s", tc.metric, tc.source, body)
			continue
		}
		if got != tc.want {
			t.Errorf("%s{source=%q} = %d, want %d (cumulative across both runs)",
				tc.metric, tc.source, got, tc.want)
		}
	}
	// A source with zero failures must not gain a phantom failure
	// series; the by(source) ratio would then read 0/N and never
	// alert, which is right, but a phantom N/0 would be worse.
	if _, ok := labelledSampleValue(t, body, "archive_completeness_repair_failures_total", "sdf-core-live-001"); ok {
		t.Errorf("healthy source gained a failure series:\n%s", body)
	}
}

// TestWriteTextfileAtomic_RoundTrip — writes via the atomic path
// (.tmp + rename) and the final file is a valid Prometheus textfile.
func TestWriteTextfileAtomic_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "archive_completeness.prom")

	snap := archivecompleteness.NewMetricsSnapshot()
	snap.FilesMissing["cross-anchor"] = 5

	if err := archivecompleteness.WriteTextfileAtomic(path, snap); err != nil {
		t.Fatalf("WriteTextfileAtomic: %v", err)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(body), `archive_files_missing{archive="cross-anchor"} 5`) {
		t.Errorf("textfile content missing expected metric:\n%s", body)
	}

	// The .tmp companion must NOT exist after a successful rename.
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf(".tmp file should be removed after rename; err=%v", err)
	}
}
