package archivecompleteness_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/archivecompleteness"
)

// C4-038 / C4-039 / C4-054 (audit-2026-07-23).
//
// `verify` rewrites the whole textfile atomically on every run and
// only sets LastSuccessTimestamp on a clean, non-vacuous run. The
// writer used to omit the last_success line whenever the timestamp
// was zero, with a comment claiming node_exporter would "surface the
// previous-scrape value". node_exporter's textfile collector re-reads
// the file on every scrape, so the SERIES DISAPPEARS instead — and
// `(time() - archive_completeness_last_success_timestamp) > 26h`
// yields no samples over an absent series, so the staleness alert
// went silent during exactly the persistent-failure state it exists
// for.

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

// TestWriteTextfileAtomic_LastSuccessSurvivesFailedRun is the core
// C4-038 regression: a clean run stamps the timestamp, a subsequent
// RESIDUAL run rewrites the file, and the last_success series must
// still be present AND still carry the clean run's value.
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

	// Runs 2 and 3 — residuals, so no fresh success timestamp. The
	// series must persist unchanged across BOTH rewrites (a
	// carry-forward that only survives one generation would still
	// blind the 48h critical-stale alert).
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

// A host that has NEVER had a clean run must publish 0 rather than
// nothing. `time() - 0` is enormous, so the staleness alert fires —
// which is correct: a never-verified archive is not a healthy one,
// and the pre-fix behaviour (no series at all) made it look like one.
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

// The repair counters carry the `_total` suffix and a
// `# TYPE … counter` declaration, and the alert rules apply
// increase() to them. Both promises are only true if the value on
// disk accumulates across runs instead of being replaced by each
// run's fragment.
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

// C4-037 at the Fill layer: a source that fails EVERY try must still
// appear in the attempt denominator, otherwise its failure ratio has
// nothing to divide by. Two sources, the first always 500, the second
// always healthy — the failing one must show attempts == failures.
func TestFill_PerSourceFailuresUseRealSourceNames(t *testing.T) {
	root := t.TempDir()

	// A checkpoint body that validateCheckpointContent accepts is
	// non-trivial to synthesise, so BOTH sources here fail — the
	// assertion under test is the label attribution, which is
	// identical either way. The healthy-source case is covered by
	// the existing cross_anchor_fill tests.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()
	notFound := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer notFound.Close()

	filler, err := archivecompleteness.NewCrossAnchorFiller(archivecompleteness.FillerOptions{
		ArchiveRoot: root,
		Workers:     1,
		Sources: []archivecompleteness.Source{
			{Name: "always-500", URL: bad.URL},
			{Name: "always-404", URL: notFound.URL},
		},
	})
	if err != nil {
		t.Fatalf("NewCrossAnchorFiller: %v", err)
	}

	res := filler.Fill(context.Background(), []uint32{63, 127})

	for _, source := range []string{"always-500", "always-404"} {
		if got := res.PerSourceAttempt[source]; got != 2 {
			t.Errorf("PerSourceAttempt[%q] = %d, want 2 (one try per missing checkpoint)", source, got)
		}
		if got := res.PerSourceFailure[source]; got != 2 {
			t.Errorf("PerSourceFailure[%q] = %d, want 2", source, got)
		}
	}
	if len(res.Failed) != 2 {
		t.Errorf("Failed = %d entries, want 2", len(res.Failed))
	}

	// And the metrics layer must carry those real names through.
	snap := archivecompleteness.NewMetricsSnapshot()
	snap.PopulateFromFillResult(res)
	for source := range snap.RepairFailures {
		if snap.RepairAttempts[source] == 0 {
			t.Errorf("failure label %q has no attempts denominator", source)
		}
	}
	if _, present := snap.RepairFailures["multi-source-exhausted"]; present {
		t.Errorf("synthetic label leaked into the snapshot: %v", snap.RepairFailures)
	}
}
