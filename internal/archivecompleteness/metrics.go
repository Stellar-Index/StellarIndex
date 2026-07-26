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
	// FilesMissing per archive. The shipped flow populates
	// `cross-anchor` only; callers may add other archives once those
	// checks are implemented.
	FilesMissing map[string]int

	// RunDurationSeconds is wall-clock for the whole verify run.
	RunDurationSeconds float64

	// LastSuccessTimestamp is set to the run's start time when
	// the run completed cleanly (no missing files left). Zero when
	// the run finished with residuals — [WriteTextfileAtomic] then
	// re-emits the PREVIOUS run's value read back off the existing
	// textfile, which is what alerts on `_last_success_timestamp
	// older than 26h` consume.
	LastSuccessTimestamp time.Time

	// RepairAttempts / RepairFailures per source, for THIS run.
	// [WriteTextfileAtomic] adds them to the totals it reads back
	// off the existing textfile, so what lands on disk is a genuine
	// monotonic counter (which is what the `_total` suffix and the
	// `# TYPE … counter` declaration promise, and what makes
	// `increase()` in the alert rules meaningful).
	RepairAttempts map[string]int
	RepairFailures map[string]int
}

// NewMetricsSnapshot returns an initialised snapshot ready to be
// populated by Run callers.
func NewMetricsSnapshot() *MetricsSnapshot {
	return &MetricsSnapshot{
		FilesMissing:   map[string]int{},
		RepairAttempts: map[string]int{},
		RepairFailures: map[string]int{},
	}
}

// WriteTextfile renders snapshot to w in Prometheus exposition
// format (the format node_exporter's textfile collector reads).
//
// Pure: it renders exactly what the snapshot holds. Cross-run
// persistence (carrying the previous run's last-success timestamp
// and repair totals forward) is [WriteTextfileAtomic]'s job, because
// only it knows the path to read the previous file back from.
//
// Operator workflow per ADR-0017 §"Prometheus surface":
//
//   - systemd timer runs `archive-completeness verify -textfile-output PATH`
//   - PATH points at node_exporter's textfile_collector directory
//     (e.g. `/var/lib/node_exporter/textfile_collector/archive_completeness.prom`)
//   - node_exporter scrapes the directory and exposes the metrics
//     on its standard /metrics endpoint
//   - Prometheus scrapes node_exporter; alerts in
//     `deploy/monitoring/rules/archive-completeness.yml` fire on
//     the threshold conditions
//
// Atomic write protocol: the caller writes to a `<PATH>.tmp` file
// first, then renames into place. node_exporter's textfile
// collector treats partial writes as a parse error and skips them,
// so renaming-after-write avoids a race where the scrape sees a
// truncated metric block.
func WriteTextfile(w io.Writer, snapshot *MetricsSnapshot) error {
	if snapshot == nil {
		return nil
	}
	if err := writeFilesMissing(w, snapshot); err != nil {
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

// writeLastSuccess ALWAYS emits the last_success_timestamp gauge.
//
// C4-038/039/054 (audit-2026-07-23): this used to return early on a
// zero timestamp, with a comment claiming node_exporter would
// "surface the previous-scrape value". It does not — the textfile
// collector re-reads the file on every scrape, so an omitted line
// means the SERIES DISAPPEARS. `archive-completeness.yml` alerts on
// `(time() - archive_completeness_last_success_timestamp) > 26h/48h`,
// and a PromQL comparison over an absent series yields no samples, so
// the staleness alert went silent during exactly the persistent-
// failure state it exists for — absence reading as health.
//
// [WriteTextfileAtomic] fills a zero timestamp from the previous
// textfile before we get here, so this normally emits the last clean
// run's value. A literal 0 means "no clean run has EVER been
// recorded on this host", which makes `time() - 0` enormous and
// correctly fires the staleness alert — a never-verified archive is
// not a healthy one.
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

// WriteTextfileAtomic writes snapshot to path via the standard
// node_exporter textfile-collector atomic-write protocol:
//
//  1. Read the existing textfile at path and fold its persistent
//     state into snapshot (see [MetricsSnapshot.carryForward])
//  2. Write to `<path>.tmp` with restrictive permissions
//  3. Rename to <path> (POSIX atomic on the same filesystem)
//
// node_exporter's textfile collector skips files whose name ends
// in `.tmp`, so a partial write never appears in a scrape.
//
// Step 1 exists because the rename REPLACES the file wholesale: any
// series this run doesn't re-emit vanishes from the next scrape.
// That is C4-037/038/039/054 — see [writeLastSuccess]. It MUTATES
// snapshot (last-success timestamp and the two repair counters) so
// that what lands on disk is cumulative host state rather than a
// per-run fragment. A missing or unparseable previous file is
// normal (first run ever, operator wiped the collector dir) and is
// treated as "no prior state", which is a plain counter reset to
// Prometheus.
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

// priorTextfileState is the subset of an already-written textfile
// that must survive a rewrite: the last clean-run timestamp and the
// two per-source repair counters. Everything else in the file
// (files-missing, run duration) describes only the run that wrote
// it and is legitimately replaced each time.
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

// maxPriorTextfileBytes bounds the read-back. The file this package
// writes is a few hundred bytes; anything vastly larger is not ours
// and we would rather start from a clean counter reset than parse an
// unbounded blob on the ops path.
const maxPriorTextfileBytes = 1 << 20

// readPriorTextfile parses the persistent state out of the textfile
// at path. Best-effort by design: a missing file (first run), an
// unreadable one, or a malformed line all degrade to "no prior
// state" rather than failing the run — an ops binary that refuses to
// publish metrics because it couldn't parse its own last output
// would recreate the blind spot this whole change removes.
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
	}
	if r.Primary != nil {
		s.FilesMissing["galexie-archive"] = r.Primary.MissingCount
	}
}

// PopulateFromFillResult merges a FillResult's per-source counts
// into the snapshot. Every try against a source — success or failure
// — contributes one attempt under that source's REAL name, and every
// failed try contributes one failure under the same name.
//
// C4-037 (audit-2026-07-23): failures used to be filed under a
// synthetic `multi-source-exhausted` label while attempts used real
// source names, so the two label sets never intersected and
// `archive-completeness.yml`'s
// `sum by (source)(failures) / sum by (source)(attempts)` alert had a
// permanently-zero numerator for every real source — structurally
// unfireable. [CrossAnchorFiller.Fill] now reports per-source
// attempts and failures directly.
//
// Per-run counts: [WriteTextfileAtomic] adds them to the totals
// already on disk so the emitted series is a real counter.
func (s *MetricsSnapshot) PopulateFromFillResult(res FillResult) {
	for source, count := range res.PerSourceAttempt {
		s.RepairAttempts[source] += count
	}
	for source, count := range res.PerSourceFailure {
		s.RepairFailures[source] += count
	}
}

// SerialiseHeader returns a stable comment-only header that
// callers can prepend to a textfile dump for human readability —
// who emitted it, when, with what tool version. node_exporter
// ignores comment lines so this is wire-safe.
func SerialiseHeader(producedBy string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Produced by %s at %s\n", producedBy, time.Now().UTC().Format(time.RFC3339))
	return b.String()
}
