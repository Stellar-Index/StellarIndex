// Binary stellarindex-sla-probe is the executable SLA-evidence
// suite. It drives load against a deployed Stellar Index API and
// reports p50 / p95 / p99 latency per endpoint, freshness against
// the currently-observed ledger, and a pass/fail verdict against
// the stated SLA targets:
//
//	p95 ≤ 200 ms
//	p99 ≤ 500 ms
//	freshness ≤ 30 s   (the price-freshness SLA, measured on
//	                    /v1/price/tip, the rolling-window surface;
//	                    /v1/price serves closed buckets per ADR-0015
//	                    and is held to a structural 150 s bound — see
//	                    defaultClosedBucketFreshTarget)
//	availability ≥ 99.9 %  (sampled per-tick error rate)
//
// Closes Codex medium-7 / Task #52 / coverage matrix rows
// S5.2, S9.1, S9.2, F3.1-F3.4. Provides the executable evidence
// the SLAs require; the rest of those rows (HA
// posture, SEV detection time) are operational SLAs that need a
// production deployment to measure, not a pre-launch CLI.
//
// Usage:
//
//	stellarindex-sla-probe \
//	    -base-url https://api.stellarindex.io/v1 \
//	    -duration 60s \
//	    -concurrency 4 \
//	    -pair native,fiat:USD \
//	    -pair USDC:GA5...,fiat:USD \
//	    -report-format json
//
// Output: a JSON report with per-endpoint statistics and overall
// pass/fail verdict. Exit code 0 = pass, 1 = at least one SLA
// violated. Designed for CI / scheduled-job integration so the
// SLA results trend over time rather than living in a one-off
// notebook.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/version"
)

// SLA targets — match the stated thresholds. Configurable via
// flags at runtime.
const (
	defaultP95Target     = 200 * time.Millisecond
	defaultP99Target     = 500 * time.Millisecond
	defaultFreshTarget   = 30 * time.Second
	defaultAvailabilityT = 99.9 // percent

	// defaultClosedBucketFreshTarget is the freshness bound applied to
	// /v1/price specifically. /v1/price serves the most recent CLOSED
	// bucket (ADR-0015 — the cross-region byte-identical surface), so
	// its observed_at is STRUCTURALLY 30–150 s old: 60 s bucket width
	// (prices_1m) + the CAGG refresh policy's 30 s end_offset + up to a
	// 30 s schedule interval + refresh runtime. The ≤30 s
	// freshness promise is served by /v1/price/tip (rolling-window,
	// sub-second observed_at) and is measured there; this bound exists
	// to catch the closed-bucket pipeline falling behind its design
	// (aggregator down, CAGG refresh job stuck, trades-insert
	// backpressure — the 2026-06-02/03 chunk-perf regression read
	// 166–186 s and would correctly fail this).
	defaultClosedBucketFreshTarget = 150 * time.Second

	// maxRequestTimeout caps one request. It must sit well inside the run
	// duration so a request that never answers times out, and is counted
	// as a failure, within the run it started in.
	maxRequestTimeout = 10 * time.Second
)

// endpoint captures one API surface to probe. Path is the URL
// suffix appended to -base-url; the runner GETs it with the
// fixed query params (if any) and counts the HTTP status code
// against the SLA's success classes (2xx).
type endpoint struct {
	Name     string
	Path     string
	Query    map[string]string
	Critical bool // when true, a single failure here fails the whole run
	// FreshTarget overrides the run-level freshness SLA target for
	// this endpoint when non-zero. Used by /price, whose closed-bucket
	// contract (ADR-0015) makes the run-level 30 s target structurally
	// unmeetable — see defaultClosedBucketFreshTarget.
	FreshTarget time.Duration
	// WantObservedAt: a 2xx without a parseable data.observed_at is a
	// failed sample. Without it a response-shape change would silently
	// drop the freshness series and the alert reading it.
	WantObservedAt bool
	// WantData: a 2xx whose `data` is null or empty is a failed sample —
	// /oracle/latest answers `{"data":[]}` when no observation exists.
	WantData bool
}

// staticEndpoints are probed regardless of -pair flags — they
// have no per-pair variant. Health + version probes verify the
// process is responsive; the catalogue probes (/assets, /issuers,
// /markets, /diagnostics/cursors) verify that the read-heavy
// surfaces the explorer site fans out across hold latency under
// load. Without these, a regression on /v1/assets would only
// surface as "the explorer is slow" — well after the SLA probe
// gate would have caught it.
//
// Migrated from /coins → /assets in rc.49: the standalone
// /v1/coins route was removed in rc.48; the asset-catalogue
// fields it surfaced are now overlay-fields on every /v1/assets
// row (rc.47 commit 578c4581). Hitting /assets keeps the same
// read-heavy fan-out coverage with the live URL.
func staticEndpoints() []endpoint {
	return []endpoint{
		{Name: "healthz", Path: "/healthz", Critical: true},
		{Name: "readyz", Path: "/readyz", Critical: true},
		{Name: "version", Path: "/version"},
		{Name: "assets", Path: "/assets", Query: map[string]string{"limit": "100"}},
		{Name: "issuers", Path: "/issuers", Query: map[string]string{"limit": "100"}},
		{Name: "markets", Path: "/markets", Query: map[string]string{"limit": "100"}},
		{Name: "diagnostics-cursors", Path: "/diagnostics/cursors"},
	}
}

// pairEndpoints expands one (asset, quote) pair into the per-pair
// endpoints we measure: /v1/price, /v1/price/tip and
// /v1/oracle/latest are the load-bearing customer surfaces;
// /v1/markets is included as a representative listing surface.
//
// The freshness SLA (≤30 s) is measured on /price/tip — the
// rolling-window surface built to deliver it. /price carries its own
// structural bound (`closedBucketFresh`) because ADR-0015's
// closed-bucket contract makes its observed_at 30–150 s old by
// design; holding it to 30 s kept the probe red for weeks with zero
// regression signal.
func pairEndpoints(asset, quote string, closedBucketFresh time.Duration) []endpoint {
	q := func(extra map[string]string) map[string]string {
		out := map[string]string{"asset": asset, "quote": quote}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	return []endpoint{
		{Name: "price", Path: "/price", Query: q(nil), Critical: true, FreshTarget: closedBucketFresh, WantObservedAt: true},
		{Name: "price-tip", Path: "/price/tip", Query: q(nil), Critical: true, WantObservedAt: true},
		{Name: "oracle-latest", Path: "/oracle/latest", Query: map[string]string{"asset": asset}, WantData: true},
	}
}

// stats holds per-endpoint sampling output.
type stats struct {
	Endpoint        string  `json:"endpoint"`
	Path            string  `json:"path"`
	Samples         int     `json:"samples"`
	Successes       int     `json:"successes"`
	Errors          int     `json:"errors"`
	AvailabilityPct float64 `json:"availability_pct"`
	// LatencyMS is computed over successful responses only, and is nil
	// when there were none: a failed request has no response latency.
	LatencyMS *latencyStats `json:"latency_ms,omitempty"`
	// ObservedAtFreshSec — for endpoints that return an observed_at
	// timestamp (price, price-tip), the STALEST response's freshness in
	// seconds, each sample measured at the instant that sample's response
	// was received (probeSample.receivedAt), not at end-of-run. The
	// promise is per response, so a median would pass a run in which 49 %
	// of reads broke it.
	// Zero when no observed_at field on this endpoint.
	ObservedAtFreshSec *float64 `json:"observed_at_fresh_sec,omitempty"`
	// FreshnessTargetSec — the per-endpoint freshness target override
	// (endpoint.FreshTarget) when one is set, so the JSON evidence
	// records which bound the verdict held this endpoint to. Zero =
	// the run-level sla.freshness_sec applied.
	FreshnessTargetSec float64 `json:"freshness_target_sec,omitempty"`
}

type latencyStats struct {
	P50  float64 `json:"p50"`
	P95  float64 `json:"p95"`
	P99  float64 `json:"p99"`
	Max  float64 `json:"max"`
	Mean float64 `json:"mean"`
}

// report is the top-level JSON output.
type report struct {
	BaseURL       string     `json:"base_url"`
	StartedAt     time.Time  `json:"started_at"`
	DurationSec   float64    `json:"duration_sec"`
	Concurrency   int        `json:"concurrency"`
	SLA           slaTargets `json:"sla"`
	PerEndpoint   []stats    `json:"per_endpoint"`
	Verdict       string     `json:"verdict"` // "pass" | "fail"
	FailedReasons []string   `json:"failed_reasons,omitempty"`
}

type slaTargets struct {
	P95MS           float64 `json:"p95_ms"`
	P99MS           float64 `json:"p99_ms"`
	FreshnessSec    float64 `json:"freshness_sec"`
	AvailabilityPct float64 `json:"availability_pct"`
}

// validateConcurrency rejects a -concurrency value that would break
// collectSamples: 0 spawns no workers (wg.Wait returns instantly with
// no samples, so every endpoint reads as failed) and a negative value
// panics on wg.Add's negative counter check.
func validateConcurrency(c int) error {
	if c <= 0 {
		return fmt.Errorf("-concurrency must be >= 1, got %d", c)
	}
	return nil
}

// probeFlags is every numeric flag, checked together by
// validateProbeFlags before any request is made.
type probeFlags struct {
	concurrency                          int
	duration, p95, p99, fresh, closedFsh time.Duration
	availability                         float64
}

// validateProbeFlags rejects numeric flags that would still produce a
// complete, plausible report: a non-positive -duration expires before the
// first request (a total-outage report from a probe that sent nothing), a
// non-positive target fails or passes every run by arithmetic, and an
// availability target outside (0, 100] is unreachable or vacuous.
func validateProbeFlags(f probeFlags) error {
	if err := validateConcurrency(f.concurrency); err != nil {
		return err
	}
	for _, d := range []struct {
		name string
		v    time.Duration
	}{
		{"-duration", f.duration},
		{"-p95-target", f.p95},
		{"-p99-target", f.p99},
		{"-freshness-target", f.fresh},
		{"-closed-bucket-freshness-target", f.closedFsh},
	} {
		if d.v <= 0 {
			return fmt.Errorf("%s must be > 0, got %v", d.name, d.v)
		}
	}
	if !(f.availability > 0 && f.availability <= 100) {
		return fmt.Errorf("-availability-target must be in (0, 100], got %v", f.availability)
	}
	return nil
}

func main() {
	// API key default falls through to STELLARINDEX_PROBE_API_KEY so
	// the systemd unit can pass it via Environment= without leaking
	// it onto the command line (visible in ps).
	defaultAPIKey := os.Getenv("STELLARINDEX_PROBE_API_KEY")

	var (
		baseURL      = flag.String("base-url", "http://localhost:3000/v1", "API base URL (required)")
		duration     = flag.Duration("duration", 30*time.Second, "Test duration")
		concurrency  = flag.Int("concurrency", 4, "Concurrent request workers")
		pairFlag     = stringSliceFlag{}
		reportFormat = flag.String("report-format", "text", "Output format: text | json")
		p95Target    = flag.Duration("p95-target", defaultP95Target, "p95 latency SLA target")
		p99Target    = flag.Duration("p99-target", defaultP99Target, "p99 latency SLA target")
		freshTarget  = flag.Duration("freshness-target", defaultFreshTarget, "Price-freshness SLA target (applied to /price/tip — the rolling-window freshness surface)")
		closedFresh  = flag.Duration("closed-bucket-freshness-target", defaultClosedBucketFreshTarget, "Freshness bound for /price, whose closed-bucket contract (ADR-0015) makes observed_at structurally 30-150s old")
		availTarget  = flag.Float64("availability-target", defaultAvailabilityT, "Per-endpoint availability SLA target (percent)")
		textfileOut  = flag.String("textfile-output", "", "Path to write Prometheus textfile (node_exporter textfile_collector format). Empty = no metrics emit.")
		apiKey       = flag.String("api-key", defaultAPIKey, "API key for Authorization: Bearer header. Defaults to $STELLARINDEX_PROBE_API_KEY. Without one the probe hits the anonymous-tier rate limit (60 req/min) and reads as a fail.")
		showVersion  = flag.Bool("version", false, "Print version and exit")
	)
	flag.Var(&pairFlag, "pair", "Asset pair as 'asset,quote' (e.g. 'native,fiat:USD'). Repeatable.")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.String())
		return
	}

	if *baseURL == "" {
		fmt.Fprintln(os.Stderr, "stellarindex-sla-probe: -base-url is required")
		flag.Usage()
		os.Exit(2)
	}

	if err := validateProbeFlags(probeFlags{
		concurrency: *concurrency, duration: *duration, availability: *availTarget,
		p95: *p95Target, p99: *p99Target, fresh: *freshTarget, closedFsh: *closedFresh,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "stellarindex-sla-probe: %v\n", err)
		os.Exit(2)
	}

	// Default pair if none supplied — XLM/USD is the headline
	// Stellar pair and a sensible smoke-test target.
	if len(pairFlag) == 0 {
		pairFlag = stringSliceFlag{"native,fiat:USD"}
	}

	endpoints := staticEndpoints()
	for _, p := range pairFlag {
		parts := strings.SplitN(p, ",", 2)
		if len(parts) != 2 {
			fmt.Fprintf(os.Stderr, "stellarindex-sla-probe: invalid -pair %q (want asset,quote)\n", p)
			os.Exit(2)
		}
		endpoints = append(endpoints, pairEndpoints(parts[0], parts[1], *closedFresh)...)
	}

	rep := runProbe(*baseURL, *apiKey, endpoints, *duration, *concurrency, slaTargets{
		P95MS:           durationMS(*p95Target),
		P99MS:           durationMS(*p99Target),
		FreshnessSec:    freshTarget.Seconds(),
		AvailabilityPct: *availTarget,
	})

	switch *reportFormat {
	case "json":
		_ = json.NewEncoder(os.Stdout).Encode(rep)
	default:
		printText(os.Stdout, &rep)
	}

	if *textfileOut != "" {
		if err := writeTextfileAtomic(*textfileOut, &rep); err != nil {
			fmt.Fprintf(os.Stderr, "stellarindex-sla-probe: write textfile: %v\n", err)
			os.Exit(2)
		}
	}

	if rep.Verdict != "pass" {
		os.Exit(1)
	}
}

// probeSample is one observation: latency + success + (optional)
// observed_at parsed from the response body, plus the instant the
// response was received.
//
// receivedAt is what freshness is measured against. It is NOT
// decoration: freshness used to be computed as time.Since(observedAt)
// during aggregation, which happens once, AFTER the whole run has
// finished — so every sample was charged the time between its own
// request and the end of the run. Over a uniformly-sampled run of
// length D that biases the MEDIAN by D/2 and the oldest sample by a
// full D. On r1 (D = 30 s) it reported /price/tip's ~0.1 s freshness
// as ~15 s, half of the 30 s page threshold spent on measurement
// error; at the SLA_PROBE_DURATION=120 s the wrapper recommends for
// memory-pressured hosts it would have read ~60 s and paged forever
// on a perfectly healthy tip.
type probeSample struct {
	latency    time.Duration
	ok         bool
	observedAt time.Time
	receivedAt time.Time
}

// runProbe drives `concurrency` workers against `endpoints` for
// `duration`, then aggregates per-endpoint stats and produces a
// pass/fail report. apiKey, when non-empty, is sent as
// `Authorization: Bearer <key>` on every request — without one the
// probe hits the anonymous-tier rate limit and the verdict reads as
// fail for reasons unrelated to actual SLA compliance.
func runProbe(baseURL, apiKey string, endpoints []endpoint, duration time.Duration, concurrency int, sla slaTargets) report {
	started := time.Now().UTC()
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()

	samples := collectSamples(ctx, baseURL, apiKey, endpoints, concurrency, min(duration, maxRequestTimeout))

	rep := report{
		BaseURL:     baseURL,
		StartedAt:   started,
		DurationSec: time.Since(started).Seconds(),
		Concurrency: concurrency,
		SLA:         sla,
	}
	for _, ep := range endpoints {
		rep.PerEndpoint = append(rep.PerEndpoint, aggregateEndpointStats(ep, samples[ep.Name]))
	}
	computeVerdict(&rep, sla)
	return rep
}

// collectSamples spawns `concurrency` workers that round-robin
// across `endpoints` until ctx expires. Returns a per-endpoint-name
// sample slice.
//
// ctx's deadline stops workers STARTING requests; it never cancels one
// in flight. Each request instead runs to completion or to its own
// reqTimeout and is counted either way. Cancelling it at the deadline
// and discarding it made a request the API accepted and never answered
// invisible: the run straddling a hang read 100 %.
func collectSamples(ctx context.Context, baseURL, apiKey string, endpoints []endpoint, concurrency int, reqTimeout time.Duration) map[string][]probeSample {
	var mu sync.Mutex
	samples := make(map[string][]probeSample)

	// Every endpoint is on the same host; the default transport's
	// MaxIdleConnsPerHost (2) would force connection churn the moment
	// concurrency > 2, and a churned keep-alive is a closed-connection
	// race waiting to happen. Size the idle pool to the worker count
	// so each worker keeps a warm connection between requests.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = concurrency * 2
	transport.MaxIdleConnsPerHost = concurrency * 2
	httpClient := &http.Client{Timeout: reqTimeout, Transport: transport}
	reqCtx := context.WithoutCancel(ctx)

	var wg sync.WaitGroup
	wg.Add(concurrency)
	for w := 0; w < concurrency; w++ {
		go func(workerID int) {
			defer wg.Done()
			i := workerID % len(endpoints)
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}
				ep := endpoints[i]
				i = (i + 1) % len(endpoints)
				lat, ok, observedAt := hit(reqCtx, httpClient, baseURL, apiKey, ep)
				// Stamp the receipt instant here, before the mutex: this
				// is the clock reading freshness is measured against, and
				// it must be the sample's own instant rather than anything
				// the end-of-run aggregation can see.
				receivedAt := time.Now()
				mu.Lock()
				samples[ep.Name] = append(samples[ep.Name], probeSample{
					latency:    lat,
					ok:         ok,
					observedAt: observedAt,
					receivedAt: receivedAt,
				})
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()
	return samples
}

// aggregateEndpointStats reduces a slice of samples into one stats
// row.
func aggregateEndpointStats(ep endpoint, ss []probeSample) stats {
	if len(ss) == 0 {
		return stats{Endpoint: ep.Name, Path: ep.Path}
	}
	// Failures stay out of the latency percentiles, as they do from the
	// server's success histogram (internal/obs/http_middleware.go): a
	// refused connection "takes" ~0 ms, so pooling it would report a hard
	// outage as a fast API. Failures are counted by availability instead.
	var latencies, freshSamples []float64
	for _, s := range ss {
		if s.ok {
			latencies = append(latencies, float64(s.latency.Milliseconds()))
		}
		// Anchored to the sample's own receipt instant, not to now():
		// aggregation runs after the whole run, so time.Since() here
		// would charge every sample the distance from its request to
		// the end of the run (see probeSample.receivedAt).
		if !s.observedAt.IsZero() && !s.receivedAt.IsZero() {
			freshSamples = append(freshSamples, s.receivedAt.Sub(s.observedAt).Seconds())
		}
	}
	successes := len(latencies)
	st := stats{
		Endpoint:           ep.Name,
		Path:               ep.Path,
		FreshnessTargetSec: ep.FreshTarget.Seconds(),
		Samples:            len(ss),
		Successes:          successes,
		Errors:             len(ss) - successes,
		AvailabilityPct:    100.0 * float64(successes) / float64(len(ss)),
	}
	if successes > 0 {
		st.LatencyMS = &latencyStats{
			P50:  percentile(latencies, 0.50),
			P95:  percentile(latencies, 0.95),
			P99:  percentile(latencies, 0.99),
			Max:  maxFloat(latencies),
			Mean: meanFloat(latencies),
		}
	}
	if len(freshSamples) > 0 {
		stalest := maxFloat(freshSamples)
		st.ObservedAtFreshSec = &stalest
	}
	return st
}

// computeVerdict scans rep.PerEndpoint against sla and fills
// rep.Verdict + rep.FailedReasons.
func computeVerdict(rep *report, sla slaTargets) {
	rep.Verdict = "pass"
	for _, st := range rep.PerEndpoint {
		rep.FailedReasons = append(rep.FailedReasons, endpointFailures(st, sla)...)
	}
	if len(rep.FailedReasons) > 0 {
		rep.Verdict = "fail"
	}
}

// endpointFailures returns the human-readable SLA-violation strings
// for one endpoint. Empty slice = endpoint passes.
func endpointFailures(st stats, sla slaTargets) []string {
	if st.Samples == 0 {
		return []string{fmt.Sprintf("%s: no samples", st.Endpoint)}
	}
	var out []string
	if st.LatencyMS != nil && st.LatencyMS.P95 > sla.P95MS {
		out = append(out, fmt.Sprintf("%s: p95=%.1fms > target %.1fms", st.Endpoint, st.LatencyMS.P95, sla.P95MS))
	}
	if st.LatencyMS != nil && st.LatencyMS.P99 > sla.P99MS {
		out = append(out, fmt.Sprintf("%s: p99=%.1fms > target %.1fms", st.Endpoint, st.LatencyMS.P99, sla.P99MS))
	}
	if st.AvailabilityPct < sla.AvailabilityPct {
		out = append(out, fmt.Sprintf("%s: availability=%.2f%% < target %.2f%%", st.Endpoint, st.AvailabilityPct, sla.AvailabilityPct))
	}
	freshTarget := sla.FreshnessSec
	if st.FreshnessTargetSec > 0 {
		freshTarget = st.FreshnessTargetSec
	}
	if st.ObservedAtFreshSec != nil && *st.ObservedAtFreshSec > freshTarget {
		out = append(out, fmt.Sprintf("%s: freshness=%.1fs > target %.1fs", st.Endpoint, *st.ObservedAtFreshSec, freshTarget))
	}
	return out
}

// hit issues one GET to `<baseURL><path>?<query>` and returns the
// wall-clock latency, success boolean (2xx), and the parsed
// observed_at timestamp from the response body when present. apiKey,
// when non-empty, is sent as `Authorization: Bearer <key>`.
func hit(ctx context.Context, c *http.Client, baseURL, apiKey string, ep endpoint) (time.Duration, bool, time.Time) {
	u := baseURL + ep.Path
	if len(ep.Query) > 0 {
		var parts []string
		for k, v := range ep.Query {
			parts = append(parts, fmt.Sprintf("%s=%s", k, v))
		}
		sort.Strings(parts)
		u = u + "?" + strings.Join(parts, "&")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, http.NoBody)
	if err != nil {
		return 0, false, time.Time{}
	}
	// The API keeps synthetic traffic out of the customer-facing SLO and
	// out of the access log by User-Agent prefix, and `stellarindex-probe/`
	// is the prefix it reserves for operator probes
	// (internal/obs.IsSyntheticUA). Sending none meant Go's default
	// `Go-http-client/1.1`, so this probe's load — ~800 requests per
	// endpoint per run, every 15 minutes — was counted as customer
	// traffic in the availability ratio and in the latency histogram, and
	// it cleared the burn alerts' own 5 req/s "don't burn on synthetic
	// traffic" floor while doing it. A probe that cannot be told apart
	// from a customer makes every number derived from the difference
	// wrong.
	req.Header.Set("User-Agent", "stellarindex-probe/1")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	start := time.Now()
	resp, err := c.Do(req)
	lat := time.Since(start)
	if err != nil {
		return lat, false, time.Time{}
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	ok := resp.StatusCode >= 200 && resp.StatusCode < 300
	if !ok {
		return lat, false, time.Time{}
	}
	observedAt, ok := checkBody(ep, body)
	return lat, ok, observedAt
}

// checkBody parses data.observed_at from a 2xx body (zero when absent)
// and reports whether the body meets ep's contract. Endpoints with no
// contract accept any body, including a non-JSON one.
func checkBody(ep endpoint, body []byte) (time.Time, bool) {
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	_ = json.Unmarshal(body, &env)
	var obj struct {
		ObservedAt time.Time `json:"observed_at"`
	}
	_ = json.Unmarshal(env.Data, &obj)
	if ep.WantObservedAt && obj.ObservedAt.IsZero() {
		return time.Time{}, false
	}
	if ep.WantData && !hasData(env.Data) {
		return obj.ObservedAt, false
	}
	return obj.ObservedAt, true
}

// hasData reports whether raw is a non-null scalar or a non-empty
// array or object.
func hasData(raw json.RawMessage) bool {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return false
	}
	switch d := v.(type) {
	case nil:
		return false
	case []any:
		return len(d) > 0
	case map[string]any:
		return len(d) > 0
	default:
		return true
	}
}

// percentile returns the p-th percentile (0..1) of xs using
// linear interpolation between rank-positions. Mutates xs (sorts
// in place); pass a copy if the caller needs to preserve order.
func percentile(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sort.Float64s(xs)
	if len(xs) == 1 {
		return xs[0]
	}
	rank := p * float64(len(xs)-1)
	lo := int(math.Floor(rank))
	hi := int(math.Ceil(rank))
	if lo == hi {
		return xs[lo]
	}
	weight := rank - float64(lo)
	return xs[lo]*(1-weight) + xs[hi]*weight
}

func maxFloat(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	m := xs[0]
	for _, x := range xs {
		if x > m {
			m = x
		}
	}
	return m
}

func meanFloat(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	return sum / float64(len(xs))
}

func durationMS(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}

// printText renders the report as a human-readable summary —
// useful for ad-hoc CLI runs without a JSON consumer.
func printText(w io.Writer, rep *report) {
	fmt.Fprintf(w, "stellarindex-sla-probe — %s\n", rep.BaseURL)
	fmt.Fprintf(w, "  duration: %.1fs   concurrency: %d   started: %s\n",
		rep.DurationSec, rep.Concurrency, rep.StartedAt.Format(time.RFC3339))
	fmt.Fprintf(w, "  SLA: p95<=%vms p99<=%vms fresh<=%vs avail>=%v%%\n\n",
		rep.SLA.P95MS, rep.SLA.P99MS, rep.SLA.FreshnessSec, rep.SLA.AvailabilityPct)
	fmt.Fprintf(w, "%-15s %-25s %7s %7s %7s %7s %9s\n",
		"endpoint", "path", "p50ms", "p95ms", "p99ms", "avail%", "fresh-s")
	for _, st := range rep.PerEndpoint {
		fresh := "—"
		if st.ObservedAtFreshSec != nil {
			fresh = fmt.Sprintf("%.1f", *st.ObservedAtFreshSec)
		}
		p50, p95, p99 := "—", "—", "—"
		if st.LatencyMS != nil {
			p50 = fmt.Sprintf("%.1f", st.LatencyMS.P50)
			p95 = fmt.Sprintf("%.1f", st.LatencyMS.P95)
			p99 = fmt.Sprintf("%.1f", st.LatencyMS.P99)
		}
		fmt.Fprintf(w, "%-15s %-25s %7s %7s %7s %6.2f%% %9s\n",
			st.Endpoint, st.Path, p50, p95, p99, st.AvailabilityPct, fresh)
	}
	fmt.Fprintf(w, "\nverdict: %s\n", rep.Verdict)
	if len(rep.FailedReasons) > 0 {
		fmt.Fprintln(w, "failed:")
		for _, r := range rep.FailedReasons {
			fmt.Fprintf(w, "  - %s\n", r)
		}
	}
}

// stringSliceFlag is the standard Go pattern for repeatable
// flags: each `-pair foo,bar` appends one entry.
type stringSliceFlag []string

func (s *stringSliceFlag) String() string { return strings.Join(*s, ",") }
func (s *stringSliceFlag) Set(v string) error {
	*s = append(*s, v)
	return nil
}
