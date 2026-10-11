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

// MetricsSnapshot is the per-run set of metrics we emit. Field
// names match the alert rules in
// `deploy/monitoring/rules/archive-completeness.yml`; renaming any
// field is a wire break against the alert rule file.
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

// NewMetricsSnapshot returns an initialised snapshot ready to be
// populated by Run callers.
func NewMetricsSnapshot() *MetricsSnapshot {
	return &MetricsSnapshot{
		FilesMissing:   map[string]int{},
		ChecksExpected: map[string]int{},
		ChecksFound:    map[string]int{},
		RepairAttempts: map[string]int{},
		RepairFailures: map[string]int{},
	}
}

// WriteTextfile renders snapshot to w in Prometheus exposition format for node_exporter's textfile
// collector (ADR-0017 "Prometheus surface"; alerts live in
// `deploy/monitoring/rules/archive-completeness.yml`).
//
// Pure: it renders exactly what the snapshot holds. Carrying the previous run's last-success
// timestamp and repair totals forward is [WriteTextfileAtomic]'s job, as only it knows the path.
// A systemd timer writes it to node_exporter's textfile_collector/archive_completeness.prom; the
// caller writes `<PATH>.tmp` then renames, since the collector treats partial writes as parse errors.
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

// WriteTextfileAtomic writes snapshot to path using the node_exporter textfile-collector protocol:
// fold the existing file's persistent state into snapshot ([MetricsSnapshot.carryForward]), write
// `<path>.tmp` with restrictive permissions, then rename (POSIX atomic on one filesystem). The
// collector skips `.tmp` files, so a partial write never appears in a scrape.
//
// The fold exists because the rename REPLACES the file wholesale, so any series this run does not
// re-emit vanishes (see [writeLastSuccess]). It MUTATES snapshot (last-success timestamp and the
// two repair counters) so the file holds cumulative host state, not a per-run fragment. A missing
// or unparseable previous file means "no prior state", a plain counter reset to Prometheus.
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

// PopulateFromReport fills snapshot.FilesMissing from a Report's
// per-archive missing-counts. Convenience for the verify-mode
// caller; lets the metrics layer stay decoupled from Report's
// internal field names.
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

// PopulateFromFillResult merges a FillResult's per-source counts into the snapshot. Every try
// (success or failure) adds one attempt under the source's REAL name, and every failed try adds
// one failure under the same name.
//
// Failures under a synthetic `multi-source-exhausted` label would leave the attempt and failure
// label sets disjoint, giving the `sum by (source)(failures) / sum by (source)(attempts)` alert a
// permanently-zero numerator. These are per-run counts; [WriteTextfileAtomic] adds them to the
// totals on disk so the series is a real counter.
func (s *MetricsSnapshot) PopulateFromFillResult(res FillResult) {
	for source, count := range res.PerSourceAttempt {
		s.RepairAttempts[source] += count
	}
	for source, count := range res.PerSourceFailure {
		s.RepairFailures[source] += count
	}
}
