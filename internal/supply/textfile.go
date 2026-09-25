package supply

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"math/big"
	"os"
	"strings"
	"time"
)

// metricLastSuccessTimestamp is the staleness key the
// `stellarindex_supply_snapshot_stale` (36 h ticket) and
// `_critical_stale` (72 h page) alerts in
// deploy/monitoring/rules/supply-snapshot.yml consume as
// `time() - <metric>`. node_exporter's textfile collector re-reads
// the whole `.prom` file on every scrape, so a write that omits
// this series DELETES it from the exposition — `time() - <missing>`
// is no-data and neither alert can ever fire again. Every write
// through this package therefore carries the previous file's
// samples for this family forward unless it emits fresh ones.
const metricLastSuccessTimestamp = "stellarindex_supply_snapshot_last_success_timestamp"

const helpLastSuccessTimestamp = "Unix timestamp of the most recent successful snapshot."

// WriteSnapshotTextfile renders a [Supply] snapshot to path in the
// Prometheus textfile-collector format used by node_exporter.
//
// pass=true → emits the success metric set including a fresh
// `last_success_timestamp` gauge that the staleness alert keys on.
// pass=false → the metric set carries no fresh timestamp, so the
// previous file's samples are carried forward (see
// [metricLastSuccessTimestamp]); the failure path proper should
// still use [WriteSnapshotFailureTextfile], which has no Supply to
// report value gauges from.
//
// Atomic write protocol matches the established pattern in
// internal/archivecompleteness/metrics.go::WriteTextfileAtomic and
// cmd/stellarindex-sla-probe/textfile.go::writeTextfileAtomic —
// `<path>.tmp` first, rename into place. node_exporter skips
// `.tmp` files, so a partial write never appears in a scrape.
func WriteSnapshotTextfile(path string, snap Supply, durationSec float64, pass bool) error {
	return writeAtomicCarryingLastSuccess(path, func(w io.Writer) error {
		return writeSnapshotMetrics(w, snap, durationSec, pass)
	})
}

// WriteSnapshotFailureTextfile is the failure-path emit. Writes
// `unit_failed=1` and the run-duration gauge; it has no successful
// run to stamp, so the `last_success_timestamp` samples already in
// `path` are carried through verbatim (nothing is emitted when no
// prior file exists, which is what the `_never_initialized` alert
// keys on). Without that carry a single failed run would erase the
// series the 36 h ticket and the 72 h page both depend on.
//
// `assetRaw` is the operator-supplied -asset flag value (e.g.
// "native" / "USDC-G…"). It is resolved to its [AssetKey] ("XLM") for
// the `unit_failed` label, so the failure sample shares one asset_key
// with the carried `last_success_timestamp` and the success path; a
// value that does not parse is labelled verbatim rather than dropped.
func WriteSnapshotFailureTextfile(path, assetRaw string, durationSec float64) error {
	assetKey, err := ParseAssetKey(assetRaw)
	if err != nil {
		assetKey = assetRaw
	}
	return writeAtomicCarryingLastSuccess(path, func(w io.Writer) error {
		return writeFailureMetrics(w, assetKey, durationSec)
	})
}

// writeSnapshotMetrics emits the full success-path metric set:
//
//	stellarindex_supply_snapshot_total_xlm{asset_key=}
//	stellarindex_supply_snapshot_circulating_xlm{asset_key=}
//	stellarindex_supply_snapshot_max_xlm{asset_key=}              (only when set)
//	stellarindex_supply_snapshot_ledger{asset_key=}
//	stellarindex_supply_snapshot_observed_at_seconds{asset_key=}
//	stellarindex_supply_snapshot_run_duration_seconds
//	stellarindex_supply_snapshot_unit_failed{asset_key=}          0
//	stellarindex_supply_snapshot_last_success_timestamp{asset_key=}
//
// XLM units (not stroops) for human-readable Grafana panels —
// stroops × 10^-7. NUMERIC stroop precision is preserved in
// asset_supply_history; the textfile loses sub-stroop precision in
// the float64 conversion, which is fine for monitoring (the alerts
// don't read sub-XLM precision).
func writeSnapshotMetrics(w io.Writer, snap Supply, durationSec float64, pass bool) error {
	asset := snap.AssetKey

	if err := writeGauge(w,
		"stellarindex_supply_snapshot_total_xlm",
		"Total supply in XLM units (stroops × 10^-7).",
		asset, stroopsToXLM(snap.TotalSupply)); err != nil {
		return err
	}
	if err := writeGauge(w,
		"stellarindex_supply_snapshot_circulating_xlm",
		"Circulating supply in XLM units.",
		asset, stroopsToXLM(snap.CirculatingSupply)); err != nil {
		return err
	}
	if snap.MaxSupply != nil {
		if err := writeGauge(w,
			"stellarindex_supply_snapshot_max_xlm",
			"Max supply in XLM units (omitted for uncapped assets).",
			asset, stroopsToXLM(snap.MaxSupply)); err != nil {
			return err
		}
	}
	if err := writeGaugeInt(w,
		"stellarindex_supply_snapshot_ledger",
		"Ledger sequence the snapshot was attributed to.",
		asset, int64(snap.LedgerSequence)); err != nil {
		return err
	}
	if err := writeGaugeInt(w,
		"stellarindex_supply_snapshot_observed_at_seconds",
		"Unix timestamp of the snapshot's observed_at.",
		asset, snap.ObservedAt.Unix()); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w,
		"# HELP stellarindex_supply_snapshot_run_duration_seconds Wall-clock duration of the most recent snapshot run.\n"+
			"# TYPE stellarindex_supply_snapshot_run_duration_seconds gauge\n"+
			"stellarindex_supply_snapshot_run_duration_seconds %.3f\n",
		durationSec); err != nil {
		return err
	}
	failed := 0
	if !pass {
		failed = 1
	}
	if err := writeGaugeInt(w,
		"stellarindex_supply_snapshot_unit_failed",
		"1 when the most recent run failed, 0 on success.",
		asset, int64(failed)); err != nil {
		return err
	}
	if pass {
		if err := writeGaugeInt(w,
			metricLastSuccessTimestamp,
			helpLastSuccessTimestamp,
			asset, time.Now().Unix()); err != nil {
			return err
		}
	}
	return nil
}

// writeFailureMetrics emits a minimal failure-path block. Only
// `unit_failed` and the run-duration gauge — no value gauges (we
// don't have a Supply to report) and no `last_success_timestamp`
// (there was no success to stamp; the previous file's samples are
// carried forward by [writeAtomicCarryingLastSuccess] instead).
func writeFailureMetrics(w io.Writer, assetKey string, durationSec float64) error {
	if err := writeGaugeInt(w,
		"stellarindex_supply_snapshot_unit_failed",
		"1 when the most recent run failed, 0 on success.",
		assetKey, 1); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w,
		"# HELP stellarindex_supply_snapshot_run_duration_seconds Wall-clock duration of the most recent snapshot run.\n"+
			"# TYPE stellarindex_supply_snapshot_run_duration_seconds gauge\n"+
			"stellarindex_supply_snapshot_run_duration_seconds %.3f\n",
		durationSec)
	return err
}

func writeGauge(w io.Writer, name, help, asset string, value float64) error {
	_, err := fmt.Fprintf(w,
		"# HELP %s %s\n# TYPE %s gauge\n%s{asset_key=%q} %.3f\n",
		name, help, name, name, asset, value)
	return err
}

func writeGaugeInt(w io.Writer, name, help, asset string, value int64) error {
	_, err := fmt.Fprintf(w,
		"# HELP %s %s\n# TYPE %s gauge\n%s{asset_key=%q} %d\n",
		name, help, name, name, asset, value)
	return err
}

// stroopsToXLM divides a stroops *big.Int by 10^7 and returns the
// XLM value as float64. Loses sub-stroop precision but the textfile
// is monitoring data, not the source of truth (asset_supply_history
// retains full NUMERIC precision).
func stroopsToXLM(stroops *big.Int) float64 {
	if stroops == nil {
		return 0
	}
	rat := new(big.Rat).SetFrac(stroops, big.NewInt(10_000_000))
	f, _ := rat.Float64() // i128:ok Prometheus textfile gauge; asset_supply_history keeps full NUMERIC precision
	return f
}

// writeAtomicCarryingLastSuccess writes `body` through [writeAtomic],
// appending the `last_success_timestamp` samples already present in
// `path` whenever `body` emits none of its own.
//
// The textfile collector serves exactly what this file contains, so
// a rewrite that drops the family retires the series and silently
// disarms the staleness alerts (they evaluate `time() - <missing>`,
// which is no-data, not "very old"). Carrying the previous samples
// through verbatim keeps the timestamp truthful — it still names the
// last genuinely successful run — so the 36 h ticket and 72 h page
// fire on schedule while runs keep failing.
//
// A prior file that exists but cannot be read is NOT overwritten:
// destroying a readable staleness key is worse than missing one
// scrape of `unit_failed`, since `_stale` still escalates on the
// surviving series while an erased one escalates never.
func writeAtomicCarryingLastSuccess(path string, body func(io.Writer) error) error {
	carried, err := readLastSuccessSamples(path)
	if err != nil {
		return err
	}
	return writeAtomic(path, func(w io.Writer) error {
		var buf bytes.Buffer
		if err := body(&buf); err != nil {
			return err
		}
		fresh, err := lastSuccessSamples(buf.Bytes())
		if err != nil {
			return err
		}
		if len(fresh) == 0 && len(carried) > 0 {
			if _, err := fmt.Fprintf(&buf, "# HELP %s %s\n# TYPE %s gauge\n",
				metricLastSuccessTimestamp, helpLastSuccessTimestamp,
				metricLastSuccessTimestamp); err != nil {
				return err
			}
			for _, line := range carried {
				if _, err := fmt.Fprintln(&buf, line); err != nil {
					return err
				}
			}
		}
		_, werr := w.Write(buf.Bytes())
		return werr
	})
}

// readLastSuccessSamples returns the `last_success_timestamp` sample
// lines of an existing textfile. A missing file yields no samples and
// no error (first run — `_never_initialized` covers that state); any
// other read error is returned so the caller leaves the file alone.
func readLastSuccessSamples(path string) ([]string, error) {
	body, err := os.ReadFile(path) //nolint:gosec // same operator-supplied path this package writes
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read textfile %q: %w", path, err)
	}
	return lastSuccessSamples(body)
}

// lastSuccessSamples extracts the sample lines (labels and value
// verbatim) of the `last_success_timestamp` family from textfile
// content, skipping `# HELP` / `# TYPE` metadata — the caller
// re-emits those so the family is declared exactly once. A scan
// error is returned rather than swallowed: reporting "no samples"
// for an unreadable file is exactly the erasure this guards.
func lastSuccessSamples(body []byte) ([]string, error) {
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		rest, ok := strings.CutPrefix(line, metricLastSuccessTimestamp)
		if !ok {
			continue
		}
		// Guard against a longer metric name sharing the prefix:
		// a sample line continues with a label set or the value.
		if rest == "" || (rest[0] != '{' && rest[0] != ' ') {
			continue
		}
		out = append(out, line)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scan textfile for %s: %w", metricLastSuccessTimestamp, err)
	}
	return out, nil
}

// writeAtomic runs `body` against a `<path>.tmp` file then renames
// into place. Mirrors the pattern in internal/archivecompleteness
// and the SLA probe.
func writeAtomic(path string, body func(io.Writer) error) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644) //nolint:gosec // operator-supplied path; collector reads world-readable files
	if err != nil {
		return fmt.Errorf("create textfile %q: %w", tmp, err)
	}
	if err := body(f); err != nil {
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
