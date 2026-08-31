package archive

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// Textfile export of the verify-archive mismatch counter (issue
// #282).
//
// WHY THIS EXISTS. `stellarindex_stellar_archive_divergence` is the
// P1 page for "our archive's bytes are not the network's bytes". It
// selects `stellarindex_verify_archive_mismatches_total`, which the
// chunk walk increments (verify_archive_chunks.go) — but the ONLY
// export path for that counter was the opt-in `-metrics-listen`
// HTTP endpoint, which neither the tier-a nor the tier-b unit
// passes, and `configs/prometheus/prometheus.r1.yml` has no
// verify-archive scrape job for it to scrape. The counter therefore
// had no producer in the deployed topology and the page could not
// fire: a real divergence surfaced only as the severity-`ticket`
// `stellarindex_verify_archive_unit_failed`. The 2026-06-11 F-1329
// repoint fixed the metric NAME but not the export path.
//
// A short-lived batch job cannot be scraped reliably (the process is
// gone before the next scrape), so this uses the same node_exporter
// textfile-collector pattern every other batch emitter in the repo
// uses (sla-probe, supply-snapshot, archive-completeness,
// timescale-jobs-probe): the run writes a `.prom` file into
// `--collector.textfile.directory` and node_exporter serves it on
// its own /metrics.
//
// Three properties make the counter usable by `increase()`:
//
//  1. CUMULATIVE. The file holds a running host total, not a
//     per-run fragment — each run adds its own increments to what
//     the previous run left on disk. The `_total` suffix and the
//     `# TYPE … counter` line promise monotonicity; a per-run
//     rewrite would break `increase()` on every clean run.
//  2. ZERO-SEEDED. All three `reason` values are emitted on every
//     run even at 0. Without a pre-existing sample, a counter whose
//     series first APPEARS at 1 and then stays flat yields
//     `increase() == 0` (there is no earlier point to subtract), so
//     the page would still not fire on the very first divergence —
//     the same "absence reads as health" trap as F-0033 /
//     C4-038 (see obs.seedBoundedLabelSeries and
//     archivecompleteness.writeLastSuccess).
//  3. AGGREGATED OVER chunk_idx. The in-process counter is labelled
//     by chunk_idx so an operator can dashboard a multi-hour walk
//     live. chunk_idx is a per-run worker index — chunk 7 covers a
//     different ledger range every night — so it carries no
//     cross-run meaning and would defeat property 2 (a mismatch on
//     a never-before-seen chunk_idx creates a brand-new series).
//     The per-chunk detail stays in journald and the state file.
//
// The `tier` label is the run's `-tier` value. It keeps the tier-a
// and tier-b units' `.prom` files from exposing the SAME label set
// through one node_exporter target, which the textfile collector
// rejects as a duplicate metric (and which would make the two units
// race on each other's carry-forward).

const (
	// verifyArchiveMismatchMetric is the counter the P1
	// `stellarindex_stellar_archive_divergence` rule selects. It is
	// also declared in internal/obs (the in-process
	// `-metrics-listen` path); the name is the contract between
	// the two and the rule files.
	verifyArchiveMismatchMetric = "stellarindex_verify_archive_mismatches_total"

	verifyArchiveMismatchHelp = "Chain breaks, sequence gaps, and checkpoint mismatches found by verify-archive, cumulative per reason across runs on this host."

	// verifyArchiveLastSuccessMetric is the unix time of the last run
	// that COMPLETED CLEANLY on this host, per tier.
	//
	// The staleness page used to read node_systemd_timer_last_trigger_
	// seconds, which is when the TIMER last fired — independent of the
	// triggered service's exit status. A job that failed every single
	// night therefore kept that gauge perfectly fresh, and the page for
	// "the archive has not been verified" was defeated by exactly the
	// scenario it names (wave-D ALERT-10).
	//
	// Only a clean exit advances this. A failed run carries the prior
	// value forward, so the gauge answers "when did verification last
	// SUCCEED", which is the question the alert asks.
	verifyArchiveLastSuccessMetric = "stellarindex_verify_archive_last_success_unix"

	verifyArchiveLastSuccessHelp = "Unix time of the last verify-archive run that completed cleanly on this host, per tier. Zero when no run has ever succeeded."

	// maxPriorTextfileBytes bounds the read-back of our own
	// previous output. The file is a few hundred bytes; anything
	// vastly larger is not ours and we would rather start from a
	// clean counter reset than parse an unbounded blob on the ops
	// path.
	maxPriorTextfileBytes = 1 << 20
)

// verifyArchiveMismatchReasons is the closed reason set
// verify_archive_chunks.go can emit. Sorted, and emitted in full on
// every write so the series exist before the first divergence.
var verifyArchiveMismatchReasons = []string{"chain", "checkpoint", "sequence"}

// collectVerifyArchiveMismatches reads THIS run's mismatch counts
// back out of the process registry, summed over chunk_idx. Reading
// the live collector (rather than threading a return value up from
// the walk) means every present and future `.Inc()` site is covered
// automatically, including the chunk-abort paths that return an
// error before the orchestrator can aggregate results.
func collectVerifyArchiveMismatches() map[string]uint64 {
	ch := make(chan prometheus.Metric, 32)
	go func() {
		obs.VerifyArchiveMismatchesTotal.Collect(ch)
		close(ch)
	}()

	totals := map[string]uint64{}
	for m := range ch {
		var pb dto.Metric
		if err := m.Write(&pb); err != nil {
			continue
		}
		reason := ""
		for _, lp := range pb.GetLabel() {
			if lp.GetName() == "reason" {
				reason = lp.GetValue()
			}
		}
		if reason == "" {
			continue
		}
		if v := pb.GetCounter().GetValue(); v > 0 {
			totals[reason] += uint64(v)
		}
	}
	return totals
}

// writeVerifyArchiveTextfile folds this run's mismatch counts into
// whatever the previous run left at path and writes the result via
// the standard node_exporter atomic-write protocol: write
// `<path>.tmp`, then rename. The collector skips `.tmp` files, so a
// partial write never appears in a scrape.
//
// tier is the run's `-tier` value; only prior samples carrying the
// SAME tier are carried forward, so re-pointing a unit at a
// different tier starts a fresh zero baseline instead of
// transplanting another tier's total onto the new series (which
// would read as a jump and false-page).
func writeVerifyArchiveTextfile(path, tier string, runTotals map[string]uint64, succeeded bool, now time.Time) error {
	totals := readPriorVerifyArchiveTextfile(path, tier)
	for reason, n := range runTotals {
		totals[reason] += n
	}

	// Carry the prior success timestamp forward; only a clean run
	// advances it. This is the whole point of the metric — see
	// verifyArchiveLastSuccessMetric.
	lastSuccess := readPriorVerifyArchiveLastSuccess(path, tier)
	if succeeded {
		lastSuccess = now.Unix()
	}

	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644) //nolint:gosec // operator-supplied path; the collector reads world-readable files
	if err != nil {
		return fmt.Errorf("create textfile %q: %w", tmp, err)
	}
	if err := renderVerifyArchiveTextfile(f, tier, totals, lastSuccess); err != nil {
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

// renderVerifyArchiveTextfile writes the counter block in Prometheus
// exposition format. Every reason in verifyArchiveMismatchReasons is
// emitted, including the ones absent from totals — that is the
// zero-seeding described in the file header, not padding.
func renderVerifyArchiveTextfile(w io.Writer, tier string, totals map[string]uint64, lastSuccess int64) error {
	if _, err := fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n",
		verifyArchiveMismatchMetric, verifyArchiveMismatchHelp, verifyArchiveMismatchMetric); err != nil {
		return err
	}
	reasons := make([]string, 0, len(totals)+len(verifyArchiveMismatchReasons))
	reasons = append(reasons, verifyArchiveMismatchReasons...)
	for reason := range totals {
		if !slices.Contains(verifyArchiveMismatchReasons, reason) {
			reasons = append(reasons, reason)
		}
	}
	slices.Sort(reasons)
	for _, reason := range reasons {
		if _, err := fmt.Fprintf(w, "%s{tier=%q,reason=%q} %d\n",
			verifyArchiveMismatchMetric, tier, reason, totals[reason]); err != nil {
			return err
		}
	}
	// Emitted on EVERY write, including failed runs (carrying the prior
	// value), so the series exists before the first success and a host
	// that has never verified reads 0 rather than vanishing.
	if _, err := fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n",
		verifyArchiveLastSuccessMetric, verifyArchiveLastSuccessHelp, verifyArchiveLastSuccessMetric); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "%s{tier=%q} %d\n",
		verifyArchiveLastSuccessMetric, tier, lastSuccess); err != nil {
		return err
	}
	return nil
}

// readPriorVerifyArchiveTextfile parses the cumulative per-reason
// totals out of a textfile this function's writer produced earlier.
//
// Best-effort by design: a missing file (first run ever, operator
// wiped the collector dir), an unreadable one, or a malformed line
// all degrade to "no prior state" rather than failing the run. An
// ops binary that refused to publish metrics because it could not
// parse its own last output would recreate the blind spot this file
// removes. To Prometheus that degradation is an ordinary counter
// reset.
// readPriorVerifyArchiveLastSuccess recovers the last clean-completion
// timestamp this writer previously recorded for tier. Best-effort for
// the same reason as the counter reader: a missing or malformed file
// degrades to "never succeeded" (0), which makes the staleness alert
// fire rather than go quiet — the safe direction for a signal whose
// whole job is to notice absence.
func readPriorVerifyArchiveLastSuccess(path, tier string) int64 {
	f, err := os.Open(path) //nolint:gosec // operator-supplied path, the same one we write
	if err != nil {
		return 0
	}
	defer func() { _ = f.Close() }()

	want := fmt.Sprintf("%s{tier=%q}", verifyArchiveLastSuccessMetric, tier)
	sc := bufio.NewScanner(io.LimitReader(f, maxPriorTextfileBytes))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, want) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		v, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || v < 0 {
			continue
		}
		return v
	}
	return 0
}

func readPriorVerifyArchiveTextfile(path, tier string) map[string]uint64 {
	totals := map[string]uint64{}
	f, err := os.Open(path) //nolint:gosec // operator-supplied path, the same one we write
	if err != nil {
		return totals
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(io.LimitReader(f, maxPriorTextfileBytes))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, labels, value, ok := parseTextfileSample(line)
		if !ok || name != verifyArchiveMismatchMetric {
			continue
		}
		if labels["tier"] != tier {
			continue
		}
		if reason := labels["reason"]; reason != "" && value > 0 {
			totals[reason] += value
		}
	}
	return totals
}

// parseTextfileSample splits one exposition-format sample line into
// its metric name, label set, and integer value. Returns ok=false
// for anything it does not fully understand — see the best-effort
// contract on readPriorVerifyArchiveTextfile.
func parseTextfileSample(line string) (name string, labels map[string]string, value uint64, ok bool) {
	fields := strings.Fields(line)
	if len(fields) != 2 {
		return "", nil, 0, false
	}
	v, err := strconv.ParseFloat(fields[1], 64)
	if err != nil || v < 0 {
		return "", nil, 0, false
	}

	head := fields[0]
	labels = map[string]string{}
	if open := strings.IndexByte(head, '{'); open >= 0 {
		if !strings.HasSuffix(head, "}") {
			return "", nil, 0, false
		}
		name = head[:open]
		for _, pair := range strings.Split(head[open+1:len(head)-1], ",") {
			eq := strings.IndexByte(pair, '=')
			if eq < 0 {
				continue
			}
			key := strings.TrimSpace(pair[:eq])
			val := strings.Trim(strings.TrimSpace(pair[eq+1:]), `"`)
			labels[key] = val
		}
	} else {
		name = head
	}
	return name, labels, uint64(v), true
}
