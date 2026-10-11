// Binary stellarindex-sla-probe is the executable SLA-evidence suite.
// It drives load against a deployed Stellar Index API and reports
// per-endpoint p50 / p95 / p99 latency, freshness against the observed
// ledger, and a pass/fail verdict against the SLA targets:
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
// Usage:
//
//	stellarindex-sla-probe -base-url https://api.stellarindex.io/v1 \
//	    -duration 60s -concurrency 4 \
//	    -pair native,fiat:USD -pair USDC:GA5...,fiat:USD \
//	    -report-format json
//
// Output: a JSON report. Exit 0 = pass, 1 = at least one SLA violated.
package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/Stellar-Index/StellarIndex/internal/version"
)

// SLA targets — match the stated thresholds.
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
	// backpressure — one chunk-perf regression read 166–186 s and would
	// correctly fail this).
	defaultClosedBucketFreshTarget = 150 * time.Second

	// maxRequestTimeout caps one request. It must sit well inside the run
	// duration so a request that never answers times out, and is counted
	// as a failure, within the run it started in.
	maxRequestTimeout = 10 * time.Second

	// defaultMaxRPS paces the whole run so it stays under the API's
	// per-key rate limit: 100 rps x 30 s = 3,000 requests, half of a
	// 6,000/min limit, leaving room for the smoke runner sharing it. An
	// unpaced run on a fast API issues 8,000+ and the 429s read as an outage.
	defaultMaxRPS = 100.0
)

// endpoint captures one API surface to probe. Path is the URL
// suffix appended to -base-url; the runner GETs it with the
// fixed query params (if any) and counts the HTTP status code
// against the SLA's success classes (2xx).
type endpoint struct {
	Name string
	// Pair disambiguates Name across -pair flags: staticEndpoints leaves
	// it empty, pairEndpoints sets it to "asset/quote". Name alone
	// collides across pairs (every pair's /price/tip is named
	// "price-tip"), so every place that indexes samples or stats by
	// endpoint identity must key on (Name, Pair), never Name alone —
	// otherwise a second -pair's samples silently merge into the
	// first's and a stale pair's freshness hides behind a fresh one's.
	Pair     string
	Path     string
	Query    map[string]string
	Critical bool // when true, a single failure here fails the whole run
	// FreshTarget overrides the run-level freshness SLA target for
	// this endpoint when non-zero. Used by /price, whose closed-bucket
	// contract (ADR-0015) makes the run-level 30 s target structurally
	// unmeetable — see defaultClosedBucketFreshTarget.
	FreshTarget time.Duration
	// FallbackFreshTarget, when non-zero, is the freshness bound for
	// responses that carry no data.window_seconds: /price/tip serves
	// those from the closed 1 m bucket or the VWAP snapshot when the pair
	// had no trade in the last 30 s, so they are structurally 61-150 s
	// old. Window responses stay held to the run-level target.
	FallbackFreshTarget time.Duration
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
	pair := asset + "/" + quote
	return []endpoint{
		{Name: "price", Pair: pair, Path: "/price", Query: q(nil), Critical: true, FreshTarget: closedBucketFresh, WantObservedAt: true},
		{Name: "price-tip", Pair: pair, Path: "/price/tip", Query: q(nil), Critical: true, FallbackFreshTarget: closedBucketFresh, WantObservedAt: true},
		{Name: "oracle-latest", Pair: pair, Path: "/oracle/latest", Query: map[string]string{"asset": asset}, WantData: true},
	}
}

// fetchNetwork reads the served network from /v1/coverage .data.network,
// the same source r1-smoke.sh uses. Any failure returns "pubnet" so an
// unreadable answer keeps every probe running.
func fetchNetwork(baseURL, apiKey string) string {
	ctx, cancel := context.WithTimeout(context.Background(), maxRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/coverage", http.NoBody)
	if err != nil {
		return "pubnet"
	}
	req.Header.Set("User-Agent", "stellarindex-probe/1")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := (&http.Client{Timeout: maxRequestTimeout}).Do(req)
	if err != nil {
		return "pubnet"
	}
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		Data struct {
			Network string `json:"network"`
		} `json:"data"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&body) != nil || body.Data.Network == "" {
		return "pubnet"
	}
	return body.Data.Network
}

// dropNoPriceEndpoints removes the native/USD price, price-tip and
// oracle-latest probes on test nets, which have no price sources
// (same network profile as r1-smoke.sh). Dropped endpoints are
// never sampled, so they are in neither side of the availability
// denominator. Any other network returns endpoints unchanged.
func dropNoPriceEndpoints(endpoints []endpoint, network string, log io.Writer) []endpoint {
	if network != "testnet" && network != "futurenet" {
		return endpoints
	}
	kept := make([]endpoint, 0, len(endpoints))
	var skipped []string
	for _, ep := range endpoints {
		if ep.Pair == "native/fiat:USD" && (ep.Name == "price" || ep.Name == "price-tip" || ep.Name == "oracle-latest") {
			skipped = append(skipped, ep.Name)
			continue
		}
		kept = append(kept, ep)
	}
	if len(skipped) > 0 {
		fmt.Fprintf(log, "stellarindex-sla-probe: SKIP %s native/USD: no price sources on %s\n", strings.Join(skipped, ", "), network)
	}
	return kept
}

// sampleKey is the samples-map key for ep: Name alone collides across
// -pair flags (see endpoint.Pair), so every endpoint carrying a Pair
// is keyed on both.
func sampleKey(ep endpoint) string {
	if ep.Pair == "" {
		return ep.Name
	}
	return ep.Name + "|" + ep.Pair
}

// stats holds per-endpoint sampling output.
type stats struct {
	Endpoint string `json:"endpoint"`
	// Pair is the "asset/quote" this row was sampled for, empty for
	// static (non-per-pair) endpoints. Distinguishes otherwise-identical
	// Endpoint rows when more than one -pair is configured.
	Pair            string  `json:"pair,omitempty"`
	Path            string  `json:"path"`
	Samples         int     `json:"samples"`
	Successes       int     `json:"successes"`
	Errors          int     `json:"errors"`
	AvailabilityPct float64 `json:"availability_pct"`
	// FailedByStatus counts failed samples by cause ("429", "5xx",
	// "timeout", "conn", "body", ...) so a failing run names why it failed.
	FailedByStatus map[string]int `json:"failed_by_status,omitempty"`
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
	// FallbackObservedAtFreshSec / FallbackTargetSec: the stalest
	// fallback-served response (see endpoint.FallbackFreshTarget) and the
	// bound it is held to, kept apart so a stale window response cannot
	// hide behind the looser fallback bound.
	FallbackObservedAtFreshSec *float64 `json:"fallback_observed_at_fresh_sec,omitempty"`
	FallbackTargetSec          float64  `json:"fallback_target_sec,omitempty"`
	// Critical mirrors endpoint.Critical: a Critical endpoint fails the
	// whole run on ANY error, independent of whether the blanket
	// availability target is still cleared. Carried through to the
	// verdict so a single /readyz blip on a 2,400-sample run cannot
	// hide behind 99.9%+ overall availability.
	Critical bool `json:"critical,omitempty"`
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
	MaxRPS        float64    `json:"max_rps"`
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
	concurrency int
	duration    time.Duration
	maxRPS      float64
}

// validateProbeFlags rejects numeric flags that would still produce a
// complete, plausible report: a non-positive -duration expires before the
// first request (a total-outage report from a probe that sent nothing).
func validateProbeFlags(f probeFlags) error {
	if err := validateConcurrency(f.concurrency); err != nil {
		return err
	}
	if f.duration <= 0 {
		return fmt.Errorf("-duration must be > 0, got %v", f.duration)
	}
	if !(f.maxRPS >= 0) {
		return fmt.Errorf("-max-rps must be >= 0, got %v", f.maxRPS)
	}
	return nil
}

// resolveAPIKey falls back to STELLARINDEX_PROBE_API_KEY after parsing so
// the key stays off argv (ps) and out of Usage, which prints flag defaults
// verbatim and which the healthchecks wrapper uploads on a parse error.
func resolveAPIKey(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	return os.Getenv("STELLARINDEX_PROBE_API_KEY")
}

func main() {
	var (
		baseURL      = flag.String("base-url", "http://localhost:3000/v1", "API base URL (required)")
		duration     = flag.Duration("duration", 30*time.Second, "Test duration")
		concurrency  = flag.Int("concurrency", 4, "Concurrent request workers")
		maxRPS       = flag.Float64("max-rps", defaultMaxRPS, "Request rate cap shared by all workers; keep a run under the API key's per-minute rate limit. 0 = unpaced")
		pairFlag     = stringSliceFlag{}
		reportFormat = flag.String("report-format", "text", "Output format: text | json")
		textfileOut  = flag.String("textfile-output", "", "Path to write Prometheus textfile (node_exporter textfile_collector format). Empty = no metrics emit.")
		apiKey       = flag.String("api-key", "", "API key for Authorization: Bearer header. Defaults to $STELLARINDEX_PROBE_API_KEY. Without one the probe hits the anonymous-tier rate limit (60 req/min) and reads as a fail.")
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
		concurrency: *concurrency, duration: *duration, maxRPS: *maxRPS,
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
		endpoints = append(endpoints, pairEndpoints(parts[0], parts[1], defaultClosedBucketFreshTarget)...)
	}

	key := resolveAPIKey(*apiKey)
	endpoints = dropNoPriceEndpoints(endpoints, fetchNetwork(*baseURL, key), os.Stderr)

	rep := runProbe(*baseURL, key, endpoints, *duration, *concurrency, *maxRPS, slaTargets{
		P95MS:           durationMS(defaultP95Target),
		P99MS:           durationMS(defaultP99Target),
		FreshnessSec:    defaultFreshTarget.Seconds(),
		AvailabilityPct: defaultAvailabilityT,
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
// decoration: computing freshness as time.Since(observedAt) during
// aggregation, which happens once, AFTER the whole run has finished,
// would charge every sample the time between its own request and the
// end of the run. Over a uniformly-sampled run of length D that biases
// the MEDIAN by D/2 and the oldest sample by a full D. On r1 (D = 30 s)
// that computation reported /price/tip's ~0.1 s freshness as ~15 s, half
// of the 30 s page threshold spent on measurement error; at the
// SLA_PROBE_DURATION=120 s the wrapper recommends for memory-pressured
// hosts it would have read ~60 s and paged forever on a perfectly
// healthy tip.
type probeSample struct {
	latency time.Duration
	ok      bool
	// failure classifies a failed sample (see hit); empty when ok.
	failure    string
	observedAt time.Time
	receivedAt time.Time
	// fallback: a tip response served from the closed bucket / snapshot
	// rather than the trade window (see endpoint.FallbackFreshTarget).
	fallback bool
}

// runProbe drives `concurrency` workers against `endpoints` for
// `duration`, then aggregates per-endpoint stats and produces a
// pass/fail report. apiKey, when non-empty, is sent as
// `Authorization: Bearer <key>` on every request — without one the
// probe hits the anonymous-tier rate limit and the verdict reads as
// fail for reasons unrelated to actual SLA compliance. maxRPS > 0 caps
// the request rate across all workers; 0 leaves them unpaced.
func runProbe(baseURL, apiKey string, endpoints []endpoint, duration time.Duration, concurrency int, maxRPS float64, sla slaTargets) report {
	started := time.Now().UTC()
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()

	var limiter *rate.Limiter
	if maxRPS > 0 {
		limiter = rate.NewLimiter(rate.Limit(maxRPS), concurrency)
	}
	samples := collectSamples(ctx, baseURL, apiKey, endpoints, concurrency, limiter, min(duration, maxRequestTimeout))

	rep := report{
		BaseURL:     baseURL,
		StartedAt:   started,
		DurationSec: time.Since(started).Seconds(),
		Concurrency: concurrency,
		MaxRPS:      maxRPS,
		SLA:         sla,
	}
	for _, ep := range endpoints {
		rep.PerEndpoint = append(rep.PerEndpoint, aggregateEndpointStats(ep, samples[sampleKey(ep)]))
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
// invisible: the run straddling a hang read 100 %. A non-nil limiter
// gates every request start; its Wait fails once the next token would
// land past ctx's deadline, which ends the worker like ctx.Done does.
func collectSamples(ctx context.Context, baseURL, apiKey string, endpoints []endpoint, concurrency int, limiter *rate.Limiter, reqTimeout time.Duration) map[string][]probeSample {
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
				if limiter != nil && limiter.Wait(ctx) != nil {
					return
				}
				ep := endpoints[i]
				i = (i + 1) % len(endpoints)
				lat, failure, observedAt, fallback := hit(reqCtx, httpClient, baseURL, apiKey, ep)
				// Stamp the receipt instant here, before the mutex: this
				// is the clock reading freshness is measured against, and
				// it must be the sample's own instant rather than anything
				// the end-of-run aggregation can see.
				receivedAt := time.Now()
				mu.Lock()
				key := sampleKey(ep)
				samples[key] = append(samples[key], probeSample{
					latency:    lat,
					ok:         failure == "",
					failure:    failure,
					observedAt: observedAt,
					receivedAt: receivedAt,
					fallback:   fallback,
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
		return stats{Endpoint: ep.Name, Pair: ep.Pair, Path: ep.Path, Critical: ep.Critical}
	}
	// Failures stay out of the latency percentiles, as they do from the
	// server's success histogram (internal/obs/http_middleware.go): a
	// refused connection "takes" ~0 ms, so pooling it would report a hard
	// outage as a fast API. Failures are counted by availability instead.
	var latencies, freshSamples, fallbackSamples []float64
	var failedBy map[string]int
	for _, s := range ss {
		if s.ok {
			latencies = append(latencies, float64(s.latency.Milliseconds()))
		} else {
			if failedBy == nil {
				failedBy = make(map[string]int)
			}
			failedBy[cmp.Or(s.failure, "unknown")]++
		}
		// Anchored to the sample's own receipt instant, not to now():
		// aggregation runs after the whole run, so time.Since() here
		// would charge every sample the distance from its request to
		// the end of the run (see probeSample.receivedAt).
		if !s.observedAt.IsZero() && !s.receivedAt.IsZero() {
			age := s.receivedAt.Sub(s.observedAt).Seconds()
			if s.fallback {
				fallbackSamples = append(fallbackSamples, age)
			} else {
				freshSamples = append(freshSamples, age)
			}
		}
	}
	successes := len(latencies)
	st := stats{
		Endpoint:           ep.Name,
		Pair:               ep.Pair,
		Path:               ep.Path,
		FreshnessTargetSec: ep.FreshTarget.Seconds(),
		FallbackTargetSec:  ep.FallbackFreshTarget.Seconds(),
		Samples:            len(ss),
		Successes:          successes,
		Errors:             len(ss) - successes,
		AvailabilityPct:    100.0 * float64(successes) / float64(len(ss)),
		FailedByStatus:     failedBy,
		Critical:           ep.Critical,
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
	if len(fallbackSamples) > 0 {
		stalest := maxFloat(fallbackSamples)
		st.FallbackObservedAtFreshSec = &stalest
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

// statsLabel is st's identity for human-readable output: the endpoint
// name alone when it has no pair, "name[pair]" when it does — needed
// so two pairs' otherwise-identical "price-tip" rows read as distinct
// lines instead of two unlabelled duplicates.
func statsLabel(st stats) string {
	if st.Pair == "" {
		return st.Endpoint
	}
	return fmt.Sprintf("%s[%s]", st.Endpoint, st.Pair)
}

// endpointFailures returns the human-readable SLA-violation strings
// for one endpoint. Empty slice = endpoint passes.
func endpointFailures(st stats, sla slaTargets) []string {
	label := statsLabel(st)
	if st.Samples == 0 {
		return []string{fmt.Sprintf("%s: no samples", label)}
	}
	var out []string
	// A Critical endpoint fails the run on any error, independent of the
	// blanket availability target: that target tolerates up to 0.1% of a
	// large sample count failing, which would let a single /readyz or
	// /price/tip outage pass silently on a long run (see doc comment on
	// endpoint.Critical).
	if st.Critical && st.Errors > 0 {
		out = append(out, fmt.Sprintf("%s: %d/%d requests failed (critical endpoint)", label, st.Errors, st.Samples))
	}
	if st.LatencyMS != nil && st.LatencyMS.P95 > sla.P95MS {
		out = append(out, fmt.Sprintf("%s: p95=%.1fms > target %.1fms", label, st.LatencyMS.P95, sla.P95MS))
	}
	if st.LatencyMS != nil && st.LatencyMS.P99 > sla.P99MS {
		out = append(out, fmt.Sprintf("%s: p99=%.1fms > target %.1fms", label, st.LatencyMS.P99, sla.P99MS))
	}
	if st.AvailabilityPct < sla.AvailabilityPct {
		out = append(out, fmt.Sprintf("%s: availability=%.2f%% < target %.2f%%%s", label, st.AvailabilityPct, sla.AvailabilityPct, dominantFailure(st.FailedByStatus)))
	}
	freshTarget := sla.FreshnessSec
	if st.FreshnessTargetSec > 0 {
		freshTarget = st.FreshnessTargetSec
	}
	if st.ObservedAtFreshSec != nil && *st.ObservedAtFreshSec > freshTarget {
		out = append(out, fmt.Sprintf("%s: freshness=%.1fs > target %.1fs", label, *st.ObservedAtFreshSec, freshTarget))
	}
	if st.FallbackObservedAtFreshSec != nil && *st.FallbackObservedAtFreshSec > st.FallbackTargetSec {
		out = append(out, fmt.Sprintf("%s: fallback freshness=%.1fs > target %.1fs", label, *st.FallbackObservedAtFreshSec, st.FallbackTargetSec))
	}
	return out
}

// dominantFailure renders the most frequent failure cause as
// " (429 x 848)", or "" when there is none; ties break by key order.
func dominantFailure(byStatus map[string]int) string {
	best, bestN := "", 0
	for k, n := range byStatus {
		if n > bestN || (n == bestN && k < best) {
			best, bestN = k, n
		}
	}
	if bestN == 0 {
		return ""
	}
	return fmt.Sprintf(" (%s x %d)", best, bestN)
}

// failureClass names a non-2xx status: 429 on its own, since a rate-limited
// probe is a probe fault rather than an outage, every other code by class.
func failureClass(code int) string {
	if code == http.StatusTooManyRequests {
		return strconv.Itoa(code)
	}
	return fmt.Sprintf("%dxx", code/100)
}

// transportFailure names a request that got no HTTP response.
func transportFailure(err error) string {
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return "timeout"
	}
	return "conn"
}

// hit issues one GET to `<baseURL><path>?<query>` and returns the
// wall-clock latency, the failure cause ("" on a 2xx meeting ep's body
// contract), and the parsed observed_at timestamp from the response body
// when present. apiKey, when non-empty, is sent as `Authorization: Bearer <key>`.
func hit(ctx context.Context, c *http.Client, baseURL, apiKey string, ep endpoint) (time.Duration, string, time.Time, bool) {
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
		return 0, "request", time.Time{}, false
	}
	// The API keeps synthetic traffic out of the customer-facing SLO and
	// out of the access log by User-Agent prefix, and `stellarindex-probe/`
	// is the prefix it reserves for operator probes
	// (internal/obs.IsSyntheticUA). Sending none meant Go's default
	// `Go-http-client/1.1`, so this probe's load — hundreds of requests per
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
		return lat, transportFailure(err), time.Time{}, false
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return lat, failureClass(resp.StatusCode), time.Time{}, false
	}
	observedAt, fallback, ok := checkBody(ep, body)
	if !ok {
		return lat, "body", observedAt, fallback
	}
	return lat, "", observedAt, fallback
}

// checkBody parses data.observed_at from a 2xx body (zero when absent)
// and reports whether the body meets ep's contract. Endpoints with no
// contract accept any body, including a non-JSON one.
func checkBody(ep endpoint, body []byte) (time.Time, bool, bool) {
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	_ = json.Unmarshal(body, &env)
	var obj struct {
		ObservedAt    time.Time `json:"observed_at"`
		WindowSeconds int       `json:"window_seconds"`
	}
	_ = json.Unmarshal(env.Data, &obj)
	fallback := ep.FallbackFreshTarget > 0 && obj.WindowSeconds == 0
	if ep.WantObservedAt && obj.ObservedAt.IsZero() {
		return time.Time{}, false, false
	}
	if ep.WantData && !hasData(env.Data) {
		return obj.ObservedAt, fallback, false
	}
	return obj.ObservedAt, fallback, true
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
			statsLabel(st), st.Path, p50, p95, p99, st.AvailabilityPct, fresh)
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
