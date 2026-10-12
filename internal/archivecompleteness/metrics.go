package archivecompleteness

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// MetricsSnapshot is the per-run metric set; field names match `deploy/monitoring/rules/archive-completeness.yml`, so renaming a field is a wire break against the alert rules.
type MetricsSnapshot struct {
	// FilesMissing per archive; only `cross-anchor` is populated today.
	FilesMissing map[string]int

	// ChecksExpected / ChecksFound are the per-archive checkpoint counts a run scanned.
	// FilesMissing has no denominator, so zero missing reads the same for 2M checkpoints or none
	// (a vacuous range, see [Report.Vacuous]); these let an alert require Expected > 0 first.
	ChecksExpected map[string]int
	ChecksFound    map[string]int

	// RunDurationSeconds is wall-clock for the whole verify run.
	RunDurationSeconds float64

	// LastSuccessTimestamp is the run's start time when it completed cleanly (no missing files
	// left); zero otherwise, in which case [WriteTextfileAtomic] re-emits the previous run's value
	// read back from the existing textfile, which the 26h staleness alert consumes.
	LastSuccessTimestamp time.Time

	// RepairAttempts / RepairFailures per source, for THIS run. [WriteTextfileAtomic] adds them to
	// the totals on disk so the series is a genuine monotonic counter, as the `_total` suffix and
	// `# TYPE counter` promise and as `increase()` in the alert rules requires.
	RepairAttempts map[string]int
	RepairFailures map[string]int
}

// NewMetricsSnapshot returns an initialised snapshot ready for Run callers to populate.
func NewMetricsSnapshot() *MetricsSnapshot {
	return &MetricsSnapshot{
		FilesMissing:   map[string]int{},
		ChecksExpected: map[string]int{},
		ChecksFound:    map[string]int{},
		RepairAttempts: map[string]int{},
		RepairFailures: map[string]int{},
	}
}

// WriteTextfile renders snapshot to w for node_exporter's textfile collector (archive_completeness.prom, ADR-0017); pure, so
// carrying prior-run state forward is [WriteTextfileAtomic]'s job; callers write `<PATH>.tmp` then rename.
func WriteTextfile(w io.Writer, snapshot *MetricsSnapshot) error {
	if snapshot == nil {
		return nil
	}
	if err := writeFilesMissing(w, snapshot); err != nil {
		return err
	}
	if err := writeGauge(w, "archive_checkpoints_expected",
		"Count of checkpoint positions this run expected to find in the named archive (GH-1095 denominator).",
		"archive", snapshot.ChecksExpected); err != nil {
		return err
	}
	if err := writeGauge(w, "archive_checkpoints_found",
		"Count of checkpoint positions this run actually found in the named archive.",
		"archive", snapshot.ChecksFound); err != nil {
		return err
	}
	if err := writeLastSuccess(w, snapshot); err != nil {
		return err
	}
	if err := writeRunDuration(w, snapshot); err != nil {
		return err
	}
	if err := writeCounter(w,
		"archive_completeness_repair_attempts_total",
		"Per-source repair attempts in this run.",
		"source", snapshot.RepairAttempts); err != nil {
		return err
	}
	return writeCounter(w,
		"archive_completeness_repair_failures_total",
		"Per-source repair failures in this run.",
		"source", snapshot.RepairFailures)
}

// writeFilesMissing emits the per-archive `archive_files_missing`
// gauge block. Help/Type emitted unconditionally so the file is a
// well-formed Prometheus textfile even with an empty map (the
// scrape sees the type declaration with no samples — valid).
func writeFilesMissing(w io.Writer, snapshot *MetricsSnapshot) error {
	if _, err := io.WriteString(w,
		"# HELP archive_files_missing Count of missing files in the named archive (ADR-0017).\n"+
			"# TYPE archive_files_missing gauge\n"); err != nil {
		return err
	}
	for _, archive := range sortedKeys(snapshot.FilesMissing) {
		if _, err := fmt.Fprintf(w,
			"archive_files_missing{archive=%q} %d\n",
			archive, snapshot.FilesMissing[archive]); err != nil {
			return err
		}
	}
	return nil
}

// writeLastSuccess ALWAYS emits the last_success_timestamp gauge. The textfile collector re-reads
// the file each scrape, so an omitted line makes the series disappear, and the
// `(time() - archive_completeness_last_success_timestamp) > 26h/48h` alert over an absent series
// yields no samples: it would go silent in exactly the persistent-failure state it exists for.
//
// [WriteTextfileAtomic] normally fills a zero timestamp from the previous textfile. A literal 0
// means no clean run was ever recorded, so `time() - 0` is enormous and correctly fires the alert.
func writeLastSuccess(w io.Writer, snapshot *MetricsSnapshot) error {
	var unix int64
	if !snapshot.LastSuccessTimestamp.IsZero() {
		unix = snapshot.LastSuccessTimestamp.Unix()
	}
	_, err := fmt.Fprintf(w,
		"# HELP archive_completeness_last_success_timestamp Unix timestamp of the most recent clean run (0 = never).\n"+
			"# TYPE archive_completeness_last_success_timestamp gauge\n"+
			"archive_completeness_last_success_timestamp %d\n",
		unix)
	return err
}

// writeRunDuration emits the run_duration gauge when a run has
// occurred (seconds > 0). Skip when zero so a stub call doesn't
// emit a meaningless 0.000.
func writeRunDuration(w io.Writer, snapshot *MetricsSnapshot) error {
	if snapshot.RunDurationSeconds <= 0 {
		return nil
	}
	_, err := fmt.Fprintf(w,
		"# HELP archive_completeness_run_duration_seconds Wall-clock duration of the most recent run.\n"+
			"# TYPE archive_completeness_run_duration_seconds gauge\n"+
			"archive_completeness_run_duration_seconds %.3f\n",
		snapshot.RunDurationSeconds)
	return err
}

// writeGauge emits a labelled gauge block, HELP/TYPE unconditionally
// (like [writeFilesMissing]) so an empty map — no archive section
// populated this run — still yields a well-formed, queryable series
// rather than the metric vanishing from the scrape.
func writeGauge(w io.Writer, name, help, labelKey string, samples map[string]int) error {
	if _, err := fmt.Fprintf(w,
		"# HELP %s %s\n# TYPE %s gauge\n",
		name, help, name); err != nil {
		return err
	}
	for _, label := range sortedKeys(samples) {
		if _, err := fmt.Fprintf(w,
			"%s{%s=%q} %d\n",
			name, labelKey, label, samples[label]); err != nil {
			return err
		}
	}
	return nil
}

// writeCounter emits a labelled counter block. Skip entirely on an
// empty map so the textfile doesn't carry orphan TYPE lines for a
// counter no run has produced yet.
func writeCounter(w io.Writer, name, help, labelKey string, samples map[string]int) error {
	if len(samples) == 0 {
		return nil
	}
	if _, err := fmt.Fprintf(w,
		"# HELP %s %s\n# TYPE %s counter\n",
		name, help, name); err != nil {
		return err
	}
	for _, label := range sortedKeys(samples) {
		if _, err := fmt.Fprintf(w,
			"%s{%s=%q} %d\n",
			name, labelKey, label, samples[label]); err != nil {
			return err
		}
	}
	return nil
}

// WriteTextfileAtomic folds the existing file's persistent state into snapshot (MUTATING it: last-success timestamp and both repair counters, since the rename replaces the file and drops un-re-emitted series), then writes `<path>.tmp` and renames; a missing or unparseable prior file means no prior state.
func WriteTextfileAtomic(path string, snapshot *MetricsSnapshot) error {
	if snapshot != nil {
		snapshot.carryForward(readPriorTextfile(path))
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644) //nolint:gosec // operator-supplied path; collector reads world-readable files
	if err != nil {
		return fmt.Errorf("create textfile %q: %w", tmp, err)
	}
	if err := WriteTextfile(f, snapshot); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename textfile %q → %q: %w", tmp, path, err)
	}
	return nil
}

// priorTextfileState is the part of a written textfile that must survive a rewrite: the last
// clean-run timestamp and the two per-source repair counters. Files-missing and run duration
// describe only their own run and are replaced.
type priorTextfileState struct {
	lastSuccess time.Time
	attempts    map[string]int
	failures    map[string]int
}

// Metric names parsed back out of a previously-written textfile.
const (
	metricLastSuccess    = "archive_completeness_last_success_timestamp"
	metricRepairAttempts = "archive_completeness_repair_attempts_total"
	metricRepairFailures = "archive_completeness_repair_failures_total"
)

// maxPriorTextfileBytes bounds the read-back. Our file is a few hundred bytes; anything vastly
// larger is not ours, and a counter reset beats parsing an unbounded blob on the ops path.
const maxPriorTextfileBytes = 1 << 20

// readPriorTextfile parses the persistent state out of the textfile at path. Best-effort by
// design: a missing, unreadable or malformed file degrades to "no prior state", because a binary
// that refuses to publish metrics after failing to parse its own last output recreates the blind
// spot this persistence removes.
func readPriorTextfile(path string) priorTextfileState {
	state := priorTextfileState{
		attempts: map[string]int{},
		failures: map[string]int{},
	}
	f, err := os.Open(path) //nolint:gosec // operator-supplied path, same one we write
	if err != nil {
		return state
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(io.LimitReader(f, maxPriorTextfileBytes))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, label, value, ok := parseSample(line)
		if !ok {
			continue
		}
		switch name {
		case metricLastSuccess:
			if value > 0 {
				state.lastSuccess = time.Unix(value, 0).UTC()
			}
		case metricRepairAttempts:
			if label != "" && value > 0 {
				state.attempts[label] += int(value)
			}
		case metricRepairFailures:
			if label != "" && value > 0 {
				state.failures[label] += int(value)
			}
		}
	}
	return state
}

// parseSample splits one Prometheus exposition line into its metric
// name, its `source="…"` label value (empty when unlabelled) and its
// integer sample value. ok=false for anything this package doesn't
// emit — floats (run duration), multi-label samples, garbage.
func parseSample(line string) (name, label string, value int64, ok bool) {
	fields := strings.Fields(line)
	if len(fields) != 2 {
		return "", "", 0, false
	}
	v, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return "", "", 0, false
	}
	name = fields[0]
	if open := strings.IndexByte(name, '{'); open >= 0 {
		labels := name[open:]
		name = name[:open]
		const prefix = `{source="`
		if !strings.HasPrefix(labels, prefix) || !strings.HasSuffix(labels, `"}`) {
			return "", "", 0, false
		}
		label = labels[len(prefix) : len(labels)-2]
		if label == "" {
			return "", "", 0, false
		}
	}
	return name, label, v, true
}

// carryForward folds a previous run's persistent state into this
// run's snapshot: the last-success timestamp is adopted when this
// run didn't produce one, and the repair counters are ADDED to so
// the emitted `_total` series is monotonic across runs.
func (s *MetricsSnapshot) carryForward(prior priorTextfileState) {
	if s.LastSuccessTimestamp.IsZero() {
		s.LastSuccessTimestamp = prior.lastSuccess
	}
	if s.RepairAttempts == nil {
		s.RepairAttempts = map[string]int{}
	}
	if s.RepairFailures == nil {
		s.RepairFailures = map[string]int{}
	}
	for source, count := range prior.attempts {
		s.RepairAttempts[source] += count
	}
	for source, count := range prior.failures {
		s.RepairFailures[source] += count
	}
}

// sortedKeys returns m's keys in ascending order. Used so the
// emitted metric blocks are stable across runs (ops are happier
// when the same scrape produces byte-identical output for an
// unchanged state).
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// PopulateFromReport fills snapshot.FilesMissing from a Report's per-archive missing-counts, keeping the metrics layer decoupled from Report's field names.
func (s *MetricsSnapshot) PopulateFromReport(r *Report) {
	if r == nil {
		return
	}
	if r.CrossAnchor != nil {
		s.FilesMissing["cross-anchor"] = r.CrossAnchor.MissingCount
		s.ChecksExpected["cross-anchor"] = r.CrossAnchor.Expected
		s.ChecksFound["cross-anchor"] = r.CrossAnchor.Found
	}
	if r.Primary != nil {
		s.FilesMissing["galexie-archive"] = r.Primary.MissingCount
		s.ChecksExpected["galexie-archive"] = r.Primary.Expected
		s.ChecksFound["galexie-archive"] = r.Primary.Found
	}
}

// PopulateFromFillResult merges per-source counts: every try adds one attempt and every failed try one failure under the source's REAL name (a synthetic label would leave the failure/attempt alert ratio with a zero numerator); per-run counts that [WriteTextfileAtomic] accumulates on disk.
func (s *MetricsSnapshot) PopulateFromFillResult(res FillResult) {
	for source, count := range res.PerSourceAttempt {
		s.RepairAttempts[source] += count
	}
	for source, count := range res.PerSourceFailure {
		s.RepairFailures[source] += count
	}
}
