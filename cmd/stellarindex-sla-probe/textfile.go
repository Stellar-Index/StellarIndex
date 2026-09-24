package main

import (
	"fmt"
	"io"
	"os"
	"sort"
	"time"
)

// writeTextfile renders rep to w in Prometheus exposition format
// (the format node_exporter's textfile collector reads).
//
// Metric set:
//
//	stellarindex_sla_probe_latency_ms{endpoint=,quantile=}     gauge
//	stellarindex_sla_probe_availability_pct{endpoint=}         gauge
//	stellarindex_sla_probe_freshness_sec{endpoint=}            gauge (only when present)
//	stellarindex_sla_probe_run_duration_seconds                gauge
//	stellarindex_sla_probe_last_pass_timestamp                 gauge (only on pass)
//	stellarindex_sla_probe_unit_failed                         gauge (1 on fail, 0 on pass)
//	stellarindex_sla_probe_samples{endpoint=}                  gauge
//
// Operator wiring per docs/operations/sla-probe.md: systemd timer
// runs the probe with -textfile-output pointing at node_exporter's
// textfile_collector dir; node_exporter scrapes the dir; Prometheus
// scrapes node_exporter.
func writeTextfile(w io.Writer, rep *report) error {
	if rep == nil {
		return nil
	}
	// Keyed on (Endpoint, Pair): with more than one -pair, every pair
	// produces a "price"/"price-tip"/"oracle-latest" row, and keying on
	// Endpoint alone would let the later pair's stats clobber the
	// earlier one's in this map.
	endpoints := make([]string, 0, len(rep.PerEndpoint))
	statsByEndpoint := make(map[string]stats, len(rep.PerEndpoint))
	for _, st := range rep.PerEndpoint {
		key := statsLabel(st)
		endpoints = append(endpoints, key)
		statsByEndpoint[key] = st
	}
	sort.Strings(endpoints)

	if err := writeLatency(w, endpoints, statsByEndpoint); err != nil {
		return err
	}
	if err := writeAvailability(w, endpoints, statsByEndpoint); err != nil {
		return err
	}
	if err := writeFreshness(w, endpoints, statsByEndpoint); err != nil {
		return err
	}
	if err := writeSamples(w, endpoints, statsByEndpoint); err != nil {
		return err
	}
	if err := writeRunMeta(w, rep); err != nil {
		return err
	}
	return writeVerdict(w, rep)
}

// promLabels renders st's endpoint identity as Prometheus label text
// (without braces), adding a `pair` label only when st.Pair is set —
// so a single-pair run's series stay unchanged and a multi-pair run's
// don't collide under one endpoint name. extra, when non-empty, is
// appended verbatim (e.g. `quantile="0.95"`).
func promLabels(st stats, extra string) string {
	labels := fmt.Sprintf("endpoint=%q", st.Endpoint)
	if st.Pair != "" {
		labels += fmt.Sprintf(",pair=%q", st.Pair)
	}
	if extra != "" {
		labels += "," + extra
	}
	return labels
}

func writeLatency(w io.Writer, endpoints []string, byEndpoint map[string]stats) error {
	if _, err := io.WriteString(w,
		"# HELP stellarindex_sla_probe_latency_ms Per-endpoint latency percentile in ms.\n"+
			"# TYPE stellarindex_sla_probe_latency_ms gauge\n"); err != nil {
		return err
	}
	for _, ep := range endpoints {
		st := byEndpoint[ep]
		// No successful response, no latency: an absent series, never a
		// 0 that p95_breach and the weekly proof would read as fast.
		if st.LatencyMS == nil {
			continue
		}
		quantiles := []struct {
			label string
			value float64
		}{
			{"0.5", st.LatencyMS.P50},
			{"0.95", st.LatencyMS.P95},
			{"0.99", st.LatencyMS.P99},
		}
		for _, q := range quantiles {
			if _, err := fmt.Fprintf(w,
				"stellarindex_sla_probe_latency_ms{%s} %.3f\n",
				promLabels(st, fmt.Sprintf("quantile=%q", q.label)), q.value); err != nil {
				return err
			}
		}
	}
	return nil
}

func writeAvailability(w io.Writer, endpoints []string, byEndpoint map[string]stats) error {
	if _, err := io.WriteString(w,
		"# HELP stellarindex_sla_probe_availability_pct Per-endpoint 2xx-success rate (percent).\n"+
			"# TYPE stellarindex_sla_probe_availability_pct gauge\n"); err != nil {
		return err
	}
	for _, ep := range endpoints {
		st := byEndpoint[ep]
		if _, err := fmt.Fprintf(w,
			"stellarindex_sla_probe_availability_pct{%s} %.3f\n",
			promLabels(st, ""), st.AvailabilityPct); err != nil {
			return err
		}
	}
	return nil
}

func writeFreshness(w io.Writer, endpoints []string, byEndpoint map[string]stats) error {
	// Skip the block entirely if no endpoint has a freshness sample —
	// emitting a TYPE line with no samples is valid, but cleaner to
	// drop empty blocks so the file is shorter on the wire.
	hasFreshness := false
	for _, ep := range endpoints {
		if byEndpoint[ep].ObservedAtFreshSec != nil {
			hasFreshness = true
			break
		}
	}
	if !hasFreshness {
		return nil
	}
	if _, err := io.WriteString(w,
		"# HELP stellarindex_sla_probe_freshness_sec Per-endpoint observed_at freshness of the stalest response in the run, in seconds.\n"+
			"# TYPE stellarindex_sla_probe_freshness_sec gauge\n"); err != nil {
		return err
	}
	for _, ep := range endpoints {
		st := byEndpoint[ep]
		if st.ObservedAtFreshSec == nil {
			continue
		}
		if _, err := fmt.Fprintf(w,
			"stellarindex_sla_probe_freshness_sec{%s} %.3f\n",
			promLabels(st, ""), *st.ObservedAtFreshSec); err != nil {
			return err
		}
	}
	return nil
}

func writeSamples(w io.Writer, endpoints []string, byEndpoint map[string]stats) error {
	if _, err := io.WriteString(w,
		"# HELP stellarindex_sla_probe_samples Per-endpoint sample count for the run.\n"+
			"# TYPE stellarindex_sla_probe_samples gauge\n"); err != nil {
		return err
	}
	for _, ep := range endpoints {
		st := byEndpoint[ep]
		if _, err := fmt.Fprintf(w,
			"stellarindex_sla_probe_samples{%s} %d\n",
			promLabels(st, ""), st.Samples); err != nil {
			return err
		}
	}
	return nil
}

func writeRunMeta(w io.Writer, rep *report) error {
	_, err := fmt.Fprintf(w,
		"# HELP stellarindex_sla_probe_run_duration_seconds Wall-clock duration of the probe run.\n"+
			"# TYPE stellarindex_sla_probe_run_duration_seconds gauge\n"+
			"stellarindex_sla_probe_run_duration_seconds %.3f\n",
		rep.DurationSec)
	return err
}

func writeVerdict(w io.Writer, rep *report) error {
	failed := 0
	if rep.Verdict != "pass" {
		failed = 1
	}
	if _, err := fmt.Fprintf(w,
		"# HELP stellarindex_sla_probe_unit_failed 1 when the most recent run failed any SLA target, 0 otherwise.\n"+
			"# TYPE stellarindex_sla_probe_unit_failed gauge\n"+
			"stellarindex_sla_probe_unit_failed %d\n",
		failed); err != nil {
		return err
	}
	if rep.Verdict == "pass" {
		_, err := fmt.Fprintf(w,
			"# HELP stellarindex_sla_probe_last_pass_timestamp Unix timestamp of the most recent passing run.\n"+
				"# TYPE stellarindex_sla_probe_last_pass_timestamp gauge\n"+
				"stellarindex_sla_probe_last_pass_timestamp %d\n",
			time.Now().Unix())
		return err
	}
	return nil
}

// writeTextfileAtomic writes rep to path via the standard
// node_exporter textfile-collector atomic-write protocol —
// `<path>.tmp` first, rename into place. Mirror of
// internal/archivecompleteness/metrics.go::WriteTextfileAtomic
// (didn't import that package because the metric set is
// probe-specific and a separate file makes future evolution easier).
func writeTextfileAtomic(path string, rep *report) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644) //nolint:gosec // operator-supplied path; collector reads world-readable files
	if err != nil {
		return fmt.Errorf("create textfile %q: %w", tmp, err)
	}
	if err := writeTextfile(f, rep); err != nil {
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
		return fmt.Errorf("rename textfile %q -> %q: %w", tmp, path, err)
	}
	return nil
}
