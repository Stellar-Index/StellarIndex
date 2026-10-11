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

// Textfile export of the verify-archive mismatch counter for the P1 stellarindex_stellar_archive_divergence page (node_exporter textfile pattern; the batch job cannot be scraped).
// Three properties keep it usable by increase(): CUMULATIVE across runs; ZERO-SEEDED for all three reason values (a series first appearing at 1 yields increase()==0);
// AGGREGATED OVER the per-run chunk_idx (a new chunk_idx would create a new series). The tier label stops tier-a and tier-b .prom files exposing duplicate label sets.

const (
	// verifyArchiveMismatchMetric is the counter the P1 divergence rule selects; the name is the contract with internal/obs and the rule files.
	verifyArchiveMismatchMetric = "stellarindex_verify_archive_mismatches_total"

	verifyArchiveMismatchHelp = "Chain breaks, sequence gaps, and checkpoint mismatches found by verify-archive, cumulative per reason across runs on this host."

	// verifyArchiveLastSuccessMetric is the unix time of the last clean run per tier. Only a clean exit advances it (a failed run carries the prior value),
	// because node_systemd_timer_last_trigger_seconds measures the timer, which stays fresh while the job fails every night.
	verifyArchiveLastSuccessMetric = "stellarindex_verify_archive_last_success_unix"

	verifyArchiveLastSuccessHelp = "Unix time of the last verify-archive run that completed cleanly on this host, per tier. Zero when no run has ever succeeded."

	// maxPriorTextfileBytes bounds the read-back of our own output; anything vastly larger is not ours, so start from a counter reset.
	maxPriorTextfileBytes = 1 << 20
)

// verifyArchiveMismatchReasons is the closed reason set
// verify_archive_chunks.go can emit. Sorted, and emitted in full on
// every write so the series exist before the first divergence.
var verifyArchiveMismatchReasons = []string{"chain", "checkpoint", "sdf-sample", "sequence"}

// collectVerifyArchiveMismatches reads THIS run's mismatch counts from the process registry, summed over chunk_idx;
// reading the live collector covers every .Inc() site, including chunk-abort paths that return before aggregation.
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

// writeVerifyArchiveTextfile folds this run's counts into the previous run's file at path and writes atomically (<path>.tmp then rename; the collector skips .tmp).
// Only prior samples with the SAME tier carry forward, so re-pointing a unit at another tier starts a zero baseline instead of a false-paging jump.
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

// readPriorVerifyArchiveLastSuccess recovers the last clean-completion timestamp for tier. A missing or malformed file reads as 0 (never succeeded),
// so the staleness alert fires rather than going quiet.
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

// readPriorVerifyArchiveTextfile parses the cumulative per-reason totals from an earlier textfile. Best-effort: a missing, unreadable or malformed file
// degrades to no prior state (a Prometheus counter reset) rather than failing the run.
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
