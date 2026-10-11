package archive

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
)

// cross-region-monitor runs cross-region-check on an interval and exports Prometheus metrics (:9479/metrics)
// instead of stdout; same fetch and analysis path as the one-shot, only the loop and sink differ.
// Alerts fire on rate(stellarindex_cross_region_divergences_total[10m]) > 0 and rate(..._fetch_errors_total[5m]) > 0.1.

func crossRegionMonitor(args []string) error { //nolint:funlen,gocognit,gocyclo // single-purpose long-running loop
	fs := flag.NewFlagSet("cross-region-monitor", flag.ContinueOnError)
	regionsCSV := fs.String("regions", "",
		"Comma-separated `name=url` pairs (required). Same format as cross-region-check.")
	pairsCSV := fs.String("pairs", "native/fiat:USD",
		"Comma-separated canonical pair strings to check")
	metric := fs.String("metric", "vwap",
		"Which endpoint to compare across regions: vwap, twap, ohlc")
	window := fs.Duration("window", 30*time.Second,
		"Closed-bucket window size (matches CAGG bucket size)")
	samples := fs.Int("samples", 1,
		"How many recent closed buckets to compare per tick. 1 is enough for steady-state monitoring; bump for noisier alerts.")
	timeout := fs.Duration("timeout", 10*time.Second,
		"Per-region HTTP request timeout")
	interval := fs.Duration("interval", 60*time.Second,
		"How often to run a check. Shouldn't be smaller than window — there's only one new closed bucket per window.")
	listen := fs.String("listen", ":9479",
		"HTTP listen address for /metrics + /healthz")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *regionsCSV == "" {
		return errors.New("-regions is required")
	}
	if *samples < 1 {
		return errors.New("-samples must be >= 1")
	}
	if *interval < time.Second {
		return errors.New("-interval must be >= 1s (don't hammer regions)")
	}

	regions, err := parseRegionList(*regionsCSV)
	if err != nil {
		return err
	}
	if len(regions) < 2 {
		// Before R2/R3 bring-up only R1 is deployed. Refusing to start
		// trains operators to disable the monitor; instead log + exit cleanly so a
		// systemd unit wrapper can stay disabled until R2/R3 land
		// and a healthy `systemctl status` reports the reason.
		_, _ = fmt.Fprintf(os.Stdout,
			"cross-region-monitor: only %d region configured; needs ≥ 2 to compare.\n"+
				"This is the pre-launch posture (R2/R3 not yet deployed). Exiting cleanly.\n"+
				"Track R2/R3 bringup in docs/architecture/ha-plan.md §10.\n",
			len(regions))
		return nil
	}

	pairs := opsutil.SplitCSV(*pairsCSV)
	if len(pairs) == 0 {
		return errors.New("-pairs parsed to empty list")
	}

	metricKind := crossRegionMetric(strings.ToLower(*metric))
	switch metricKind {
	case metricVWAP, metricTWAP, metricOHLC:
	default:
		return fmt.Errorf("-metric must be one of vwap|twap|ohlc; got %q", *metric)
	}

	// Build a private Prometheus registry so we don't share with the
	// indexer/api process metrics that internal/obs registers at init.
	reg := prometheus.NewRegistry()
	exp := newCrossRegionExporter(reg)

	httpClient := &http.Client{Timeout: *timeout}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Run an immediate first check before serving so the first scrape
	// has fresh data instead of zero gauges.
	runOneTick(ctx, httpClient, regions, pairs, metricKind, *window, *samples, exp)

	// Background tick loop.
	go func() {
		ticker := time.NewTicker(*interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				runOneTick(ctx, httpClient, regions, pairs, metricKind, *window, *samples, exp)
			}
		}
	}()

	// HTTP server with /metrics + /healthz.
	staleAfter := healthStaleAfter(*interval, *timeout)
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		if code, msg := crossRegionHealth(exp, time.Now(), staleAfter); code != http.StatusOK {
			http.Error(w, msg, code)
			return
		}
		_, _ = fmt.Fprintln(w, "ok")
	})

	srv := &http.Server{
		Addr:              *listen,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	_, _ = fmt.Fprintf(os.Stderr,
		"cross-region-monitor: listening on %s; checking %d region(s) × %d pair(s) every %s\n",
		*listen, len(regions), len(pairs), *interval)
	return srv.ListenAndServe()
}

// healthStaleAfter is how long /healthz tolerates tick silence: three intervals plus one region timeout,
// derived from -interval/-timeout so two skipped ticks stay green and a third does not.
func healthStaleAfter(interval, timeout time.Duration) time.Duration {
	return 3*interval + timeout
}

// crossRegionHealth is the /healthz verdict. "Has run once" would be a latch, so it uses two recency-bounded
// facts: lastRunUnix (a sweep completed) and lastReachedUnix (a sweep reached a region, via allFailed();
// partial failure stays green and is reported by the fetch_errors metric).
func crossRegionHealth(exp *crossRegionExporter, now time.Time, staleAfter time.Duration) (int, string) {
	last := exp.lastRunUnix.Load()
	if last == 0 {
		return http.StatusServiceUnavailable, "no check sweep has completed yet"
	}
	if age := now.Sub(time.Unix(last, 0)); age > staleAfter {
		return http.StatusServiceUnavailable, fmt.Sprintf(
			"last check sweep completed %s ago (> %s) — the tick loop is stuck or dead",
			age.Truncate(time.Second), staleAfter)
	}

	reached := exp.lastReachedUnix.Load()
	if reached == 0 {
		return http.StatusServiceUnavailable,
			"no check sweep has reached any region — every region fetch is failing"
	}
	if age := now.Sub(time.Unix(reached, 0)); age > staleAfter {
		return http.StatusServiceUnavailable, fmt.Sprintf(
			"no check sweep has reached any region for %s (> %s) — every region fetch is failing",
			age.Truncate(time.Second), staleAfter)
	}
	return http.StatusOK, "ok"
}

// runOneTick does a single sweep over (pairs × samples), updates the
// metrics, and returns. Errors are recorded in counters, not propagated.
func runOneTick(
	ctx context.Context,
	client *http.Client,
	regions []regionEndpoint,
	pairs []string,
	metric crossRegionMetric,
	window time.Duration,
	samples int,
	exp *crossRegionExporter,
) {
	exp.inFlight.Inc()
	defer exp.inFlight.Dec()

	anchor, err := resolveAnchor("", window)
	if err != nil {
		exp.checkErrors.WithLabelValues(metric.String(), err.Error()).Inc()
		return
	}

	// reached records whether ANY comparison in this sweep got data back
	// from at least one region — the second liveness fact /healthz needs
	// (see crossRegionHealth). Tracked per sweep, not per comparison, so
	// one bad pair among many doesn't declare the monitor blind.
	reached := false

	for _, pair := range pairs {
		for i := 0; i < samples; i++ {
			bucketTo := anchor.Add(-time.Duration(i) * window)
			bucketFrom := bucketTo.Add(-window)

			results := fetchAllRegions(ctx, client,
				regions, pair, metric, bucketFrom, bucketTo)

			// Account for fetch failures per region.
			for _, r := range results {
				if r.Err != nil {
					exp.fetchErrors.WithLabelValues(r.Region, pair, metric.String()).Inc()
				}
			}

			// Discard the analysis stdout: divergence is in the metric labels, and compared feeds tickOutcome's
			// "inconclusive" label (the "error" outcome stays scoped to allFailed()).
			divergence, compared := analyseRegionResults(metric, pair, bucketFrom, bucketTo, results, io.Discard)

			outcome := tickOutcome(divergence, compared, allFailed(results))
			if divergence {
				exp.divergences.WithLabelValues(pair, metric.String()).Inc()
			}
			if !allFailed(results) {
				// `reached` means "we contacted a region", not "we compared" (pinned by TestRunOneTick_ReachedStampedOnPartialFailure);
				// the outcome label carries the compared fact.
				reached = true
			}
			exp.checksTotal.WithLabelValues(pair, metric.String(), outcome).Inc()
		}
	}

	now := time.Now()
	exp.lastRunUnix.Store(now.Unix())
	exp.lastRunGauge.SetToCurrentTime()
	if reached {
		exp.lastReachedUnix.Store(now.Unix())
		exp.lastReachedGauge.SetToCurrentTime()
	}
}

// allFailed reports whether every region's fetch returned an error.
// Used to flag the "every region is unreachable" alert as distinct
// from "we got data and it diverged".
func allFailed(results []regionResult) bool {
	if len(results) == 0 {
		return false
	}
	for _, r := range results {
		if r.Err == nil {
			return false
		}
	}
	return true
}

// crossRegionExporter holds the metric vectors. One instance per
// process; passed by pointer to runOneTick.
type crossRegionExporter struct {
	checksTotal      *prometheus.CounterVec
	divergences      *prometheus.CounterVec
	fetchErrors      *prometheus.CounterVec
	checkErrors      *prometheus.CounterVec
	lastRunGauge     prometheus.Gauge
	lastReachedGauge prometheus.Gauge
	inFlight         prometheus.Gauge

	// lastRunUnix and lastReachedUnix (last sweep / last sweep reaching a region) feed /healthz; the gauges above are their Prometheus twins.
	lastRunUnix     atomic.Int64
	lastReachedUnix atomic.Int64
}

func newCrossRegionExporter(reg prometheus.Registerer) *crossRegionExporter {
	exp := &crossRegionExporter{
		checksTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "stellarindex_cross_region_checks_total",
				Help: "Count of cross-region check sweeps, labelled by pair, metric, and outcome (ok | divergence | error).",
			},
			[]string{"pair", "metric", "outcome"},
		),
		divergences: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "stellarindex_cross_region_divergences_total",
				Help: "Count of buckets where regions disagreed. Alert when rate > 0 for >5m.",
			},
			[]string{"pair", "metric"},
		),
		fetchErrors: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "stellarindex_cross_region_fetch_errors_total",
				Help: "Per-region HTTP fetch failures (timeout, 5xx, parse error).",
			},
			[]string{"region", "pair", "metric"},
		),
		checkErrors: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "stellarindex_cross_region_check_errors_total",
				Help: "Internal errors in the monitor loop (e.g. anchor resolution failed).",
			},
			[]string{"metric", "reason"},
		),
		lastRunGauge: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "stellarindex_cross_region_last_run_timestamp_seconds",
				Help: "Unix time of the last completed check sweep.",
			},
		),
		lastReachedGauge: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "stellarindex_cross_region_last_reached_timestamp_seconds",
				Help: "Unix time of the last check sweep that reached at least one region. Lagging behind _last_run_ means the loop is turning but every region fetch is failing — the monitor is blind. /healthz reports 503 on the same condition.",
			},
		),
		inFlight: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "stellarindex_cross_region_check_in_flight",
				Help: "1 while a check sweep is running, 0 otherwise. Persistent 1 indicates the loop is stuck.",
			},
		),
	}
	reg.MustRegister(
		exp.checksTotal,
		exp.divergences,
		exp.fetchErrors,
		exp.checkErrors,
		exp.lastRunGauge,
		exp.lastReachedGauge,
		exp.inFlight,
	)
	return exp
}

// String makes crossRegionMetric usable as a Prometheus label value.
func (m crossRegionMetric) String() string { return string(m) }

// tickOutcome labels one (pair, metric) sweep. "inconclusive" (compared=false: fewer than two regions
// responded) must not read as "ok", or one region down would look like genuine agreement with zero
// comparisons; the one-shot treats it as a hard failure.
func tickOutcome(divergence, compared, allFailed bool) string {
	switch {
	case allFailed:
		return "error"
	case divergence:
		return "divergence"
	case !compared:
		return "inconclusive"
	default:
		return "ok"
	}
}
