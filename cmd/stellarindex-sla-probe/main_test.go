package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPercentile_LinearInterp(t *testing.T) {
	cases := []struct {
		name string
		xs   []float64
		p    float64
		want float64
	}{
		{"empty", nil, 0.5, 0},
		{"single", []float64{42}, 0.95, 42},
		{"sorted-five-p50", []float64{1, 2, 3, 4, 5}, 0.50, 3},
		{"sorted-five-p95", []float64{1, 2, 3, 4, 5}, 0.95, 4.8},
		{"unsorted", []float64{5, 1, 3, 2, 4}, 0.50, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := percentile(append([]float64(nil), tc.xs...), tc.p)
			if abs(got-tc.want) > 1e-9 {
				t.Errorf("percentile(%v, %g) = %g, want %g", tc.xs, tc.p, got, tc.want)
			}
		})
	}
}

func TestValidateConcurrency_RejectsZeroAndNegative(t *testing.T) {
	cases := []struct {
		name    string
		c       int
		wantErr bool
	}{
		{"negative", -1, true},
		{"zero", 0, true},
		{"one", 1, false},
		{"positive", 8, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateConcurrency(tc.c)
			if tc.wantErr && err == nil {
				t.Fatalf("validateConcurrency(%d) = nil, want error", tc.c)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("validateConcurrency(%d) = %v, want nil", tc.c, err)
			}
		})
	}
}

func TestRunProbe_PassPath(t *testing.T) {
	// Fake API: every request returns 200 + a healthz-shaped body
	// + an observed_at near now (so freshness < 30s).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"observed_at":"` + time.Now().UTC().Format(time.RFC3339) + `","price":"1.0"}}`))
	}))
	defer srv.Close()

	endpoints := []endpoint{
		{Name: "healthz", Path: "/healthz"},
		{Name: "price", Path: "/price", Query: map[string]string{"asset": "native", "quote": "fiat:USD"}},
	}
	rep := runProbe(srv.URL, "", endpoints, 200*time.Millisecond, 2, slaTargets{
		P95MS:           500, // very generous so the test isn't flaky
		P99MS:           1000,
		FreshnessSec:    30,
		AvailabilityPct: 99.0,
	})
	if rep.Verdict != "pass" {
		t.Errorf("verdict=%q want pass; failed=%v", rep.Verdict, rep.FailedReasons)
	}
	if len(rep.PerEndpoint) != 2 {
		t.Fatalf("PerEndpoint len=%d want 2", len(rep.PerEndpoint))
	}
	for _, st := range rep.PerEndpoint {
		if st.Samples == 0 {
			t.Errorf("%s: no samples collected", st.Endpoint)
		}
		if st.AvailabilityPct < 99.0 {
			t.Errorf("%s: availability=%g unexpectedly low", st.Endpoint, st.AvailabilityPct)
		}
	}
}

func TestRunProbe_FailsOnSlowEndpoint(t *testing.T) {
	// Fake API that delays 600ms — definitely > 200ms p95 target.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(50 * time.Millisecond) // moderate; we set tight target below to simulate fail
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	endpoints := []endpoint{{Name: "healthz", Path: "/healthz"}}
	rep := runProbe(srv.URL, "", endpoints, 200*time.Millisecond, 2, slaTargets{
		P95MS:           1, // 1ms target — we'll definitely exceed
		P99MS:           1,
		FreshnessSec:    30,
		AvailabilityPct: 99.0,
	})
	if rep.Verdict == "pass" {
		t.Errorf("verdict=pass but slow endpoint should fail tight latency target")
	}
	if len(rep.FailedReasons) == 0 {
		t.Errorf("FailedReasons empty but verdict=fail")
	}
}

func TestRunProbe_FailsOn5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	endpoints := []endpoint{{Name: "healthz", Path: "/healthz"}}
	rep := runProbe(srv.URL, "", endpoints, 100*time.Millisecond, 1, slaTargets{
		P95MS:           1000,
		P99MS:           5000,
		FreshnessSec:    300,
		AvailabilityPct: 99.0,
	})
	if rep.Verdict == "pass" {
		t.Errorf("verdict=pass but 5xx should fail availability")
	}
	if rep.PerEndpoint[0].AvailabilityPct >= 1 {
		t.Errorf("availability=%g but server always 500'd", rep.PerEndpoint[0].AvailabilityPct)
	}
}

// TestRunProbe_DeadlineCancelledSamplesNotCountedAsFailures proves
// that the run-duration deadline does not turn a request
// still in flight into a failure. The fake server here always
// returns 200 after a delay that guarantees every worker is
// mid-request when the short run window closes — so any
// availability < 100% would be a phantom failure the probe inflicted
// on itself. This is exactly what tripped
// stellarindex_sla_probe_unit_failed on r1 (the slowest endpoint,
// /v1/issuers, took the blame because it held the widest in-flight
// window). The deadline stops new requests; in-flight ones complete
// and count, which TestRunProbe_RequestHangingAtDeadlineIsAFailure
// pins from the other side.
func TestRunProbe_DeadlineCancelledSamplesNotCountedAsFailures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Long enough that at end-of-run every worker is reliably
		// still inside c.Do() when the run ctx is cancelled.
		time.Sleep(40 * time.Millisecond)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	endpoints := []endpoint{{Name: "healthz", Path: "/healthz"}}
	rep := runProbe(srv.URL, "", endpoints, 150*time.Millisecond, 4, slaTargets{
		P95MS:           10000, // generous — this test is about availability, not latency
		P99MS:           10000,
		FreshnessSec:    30,
		AvailabilityPct: 99.0,
	})
	if len(rep.PerEndpoint) != 1 {
		t.Fatalf("PerEndpoint len=%d want 1", len(rep.PerEndpoint))
	}
	st := rep.PerEndpoint[0]
	if st.AvailabilityPct != 100 {
		t.Errorf("availability=%g want 100 — the server never errored; "+
			"requests in flight at the run deadline must complete, not be counted as failures",
			st.AvailabilityPct)
	}
	if st.Samples == 0 {
		t.Error("no samples recorded — the run deadline must stop new requests, not lose the ones already made")
	}
}

func TestHit_ParsesObservedAt(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"observed_at":"` + now.Format(time.RFC3339) + `"}}`))
	}))
	defer srv.Close()
	c := &http.Client{Timeout: time.Second}
	_, ok, observed := hit(context.Background(), c, srv.URL, "", endpoint{Path: "/x"})
	if !ok {
		t.Fatal("hit returned not-ok")
	}
	if !observed.Equal(now) {
		t.Errorf("observed=%v want %v", observed, now)
	}
}

func TestHit_NoObservedAt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`)) // no data.observed_at
	}))
	defer srv.Close()
	c := &http.Client{Timeout: time.Second}
	_, ok, observed := hit(context.Background(), c, srv.URL, "", endpoint{Path: "/x"})
	if !ok {
		t.Fatal("hit returned not-ok on 200")
	}
	if !observed.IsZero() {
		t.Errorf("observed=%v want zero", observed)
	}
}

func TestHit_AttachesAuthorizationWhenAPIKeySet(t *testing.T) {
	var sawAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := &http.Client{Timeout: time.Second}
	_, ok, _ := hit(context.Background(), c, srv.URL, "sip_test_xyz", endpoint{Path: "/x"})
	if !ok {
		t.Fatal("hit returned not-ok")
	}
	if sawAuth != "Bearer sip_test_xyz" {
		t.Errorf("Authorization = %q, want %q", sawAuth, "Bearer sip_test_xyz")
	}
}

func TestHit_OmitsAuthorizationWhenAPIKeyEmpty(t *testing.T) {
	var sawAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := &http.Client{Timeout: time.Second}
	_, _, _ = hit(context.Background(), c, srv.URL, "", endpoint{Path: "/x"})
	if sawAuth != "" {
		t.Errorf("Authorization = %q, want empty (no key passed)", sawAuth)
	}
}

func TestStaticEndpoints_AllCriticalIncluded(t *testing.T) {
	es := staticEndpoints()
	want := map[string]bool{"healthz": false, "readyz": false, "version": false}
	for _, e := range es {
		want[e.Name] = true
	}
	for n, found := range want {
		if !found {
			t.Errorf("staticEndpoints missing %q", n)
		}
	}
}

func TestPairEndpoints_BuildsExpected(t *testing.T) {
	es := pairEndpoints("native", "fiat:USD", defaultClosedBucketFreshTarget)
	names := make(map[string]bool)
	for _, e := range es {
		names[e.Name] = true
		if e.Query["asset"] != "native" {
			t.Errorf("%s: asset=%q want native", e.Name, e.Query["asset"])
		}
		switch e.Name {
		case "price":
			// /price is the ADR-0015 closed-bucket surface — it must
			// carry the structural freshness override, NOT the 30 s
			// SLA target (which it can never meet by design).
			if e.FreshTarget != defaultClosedBucketFreshTarget {
				t.Errorf("price: FreshTarget=%v want %v", e.FreshTarget, defaultClosedBucketFreshTarget)
			}
		case "price-tip":
			// /price/tip is the freshness SLA surface — it must use
			// the run-level target (no override).
			if e.FreshTarget != 0 {
				t.Errorf("price-tip: FreshTarget=%v want 0 (run-level SLA target)", e.FreshTarget)
			}
		}
	}
	if !names["price"] {
		t.Error("pair endpoints missing 'price'")
	}
	if !names["price-tip"] {
		t.Error("pair endpoints missing 'price-tip'")
	}
}

// TestEndpointFailures_PerEndpointFreshnessOverride pins the
// ADR-0015 split: a closed-bucket /price observation 80 s stale is
// within its structural bound (no failure), while the same 80 s on
// /price/tip — the ≤30 s SLA surface — fails.
func TestEndpointFailures_PerEndpointFreshnessOverride(t *testing.T) {
	sla := slaTargets{P95MS: 1000, P99MS: 1000, FreshnessSec: 30, AvailabilityPct: 99.0}
	fresh := 80.0
	base := stats{
		Samples: 10, Successes: 10, AvailabilityPct: 100,
		ObservedAtFreshSec: &fresh,
	}

	price := base
	price.Endpoint = "price"
	price.FreshnessTargetSec = defaultClosedBucketFreshTarget.Seconds()
	if got := endpointFailures(price, sla); len(got) != 0 {
		t.Errorf("price at 80s within 150s structural bound should pass, got %v", got)
	}

	tip := base
	tip.Endpoint = "price-tip"
	if got := endpointFailures(tip, sla); len(got) != 1 {
		t.Errorf("price-tip at 80s must fail the 30s freshness SLA target, got %v", got)
	}

	// And the structural bound still catches a real pipeline
	// regression (the 2026-06-02/03 chunk-perf incident read 166-186s).
	regressed := 170.0
	price.ObservedAtFreshSec = &regressed
	if got := endpointFailures(price, sla); len(got) != 1 {
		t.Errorf("price at 170s must fail the 150s structural bound, got %v", got)
	}
}

// TestEndpointFailures_CriticalEndpointFailsOnAnyError proves the
// doc-comment contract on endpoint.Critical ("a single failure here
// fails the whole run"): 2 errors out of 2400 /readyz samples clears
// the blanket 99.9% availability target (99.917%) but must still fail
// because /readyz is Critical. Without the Critical hard-fail branch
// this run reads "pass".
func TestEndpointFailures_CriticalEndpointFailsOnAnyError(t *testing.T) {
	sla := slaTargets{P95MS: 1000, P99MS: 1000, FreshnessSec: 30, AvailabilityPct: 99.9}

	readyz := stats{
		Endpoint:        "readyz",
		Critical:        true,
		Samples:         2400,
		Successes:       2398,
		Errors:          2,
		AvailabilityPct: 100.0 * 2398.0 / 2400.0, // 99.9167% >= 99.9% target
	}
	if readyz.AvailabilityPct < sla.AvailabilityPct {
		t.Fatalf("test setup broken: availability %.4f must clear the blanket target %.2f", readyz.AvailabilityPct, sla.AvailabilityPct)
	}
	got := endpointFailures(readyz, sla)
	if len(got) == 0 {
		t.Fatalf("critical endpoint readyz with 2 errors must fail the run even though availability clears the blanket target, got no failures")
	}

	// A non-critical endpoint with the identical error/availability
	// profile must NOT be hard-failed by this branch — only the
	// blanket availability/latency/freshness checks apply to it.
	assets := readyz
	assets.Endpoint = "assets"
	assets.Critical = false
	if got := endpointFailures(assets, sla); len(got) != 0 {
		t.Errorf("non-critical endpoint at 99.92%% availability should pass, got %v", got)
	}
}

// JSON round-trip sanity for the report shape — anything that
// silently breaks the JSON output would surface here.
func TestReport_JSONRoundTrip(t *testing.T) {
	rep := report{
		BaseURL:     "https://api.example.com/v1",
		StartedAt:   time.Now().UTC(),
		DurationSec: 30,
		Concurrency: 4,
		SLA:         slaTargets{P95MS: 200, P99MS: 500, FreshnessSec: 30, AvailabilityPct: 99.9},
		PerEndpoint: []stats{
			{
				Endpoint: "price", Path: "/price", Samples: 100, Successes: 100, AvailabilityPct: 100,
				LatencyMS: &latencyStats{P50: 12, P95: 45, P99: 78, Max: 102, Mean: 18},
			},
		},
		Verdict: "pass",
	}
	b, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"verdict":"pass"`) {
		t.Errorf("marshalled JSON missing verdict: %s", b)
	}
	var rt report
	if err := json.Unmarshal(b, &rt); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if rt.PerEndpoint[0].LatencyMS.P95 != 45 {
		t.Errorf("round-trip lost p95: %v", rt.PerEndpoint[0])
	}
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

// TestRunProbe_FreshnessMeasuredAtSampleTime is the end-to-end guard for
// the r1 2026-09-05 reading: the probe recorded
// stellarindex_sla_probe_freshness_sec{endpoint="price-tip"} ≈ 15 s
// every run while the live tip served an observed_at that was
// sub-second old. Freshness was computed as time.Since(observedAt)
// inside aggregateEndpointStats, which runs ONCE after the whole run,
// so each sample was charged the gap between its own request and the
// end of the run — a median bias of duration/2.
//
// The fake API stamps observed_at at the instant it answers, so the
// TRUE freshness of every sample is the localhost round trip: well
// under a millisecond. Anything near duration/2 is the bias.
//
// PROVEN-RED: restore
//
//	freshSamples = append(freshSamples, time.Since(s.observedAt).Seconds())
//
// in aggregateEndpointStats and this reports ~1.0 s against a 2 s run.
func TestRunProbe_FreshnessMeasuredAtSampleTime(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"observed_at":"` +
			time.Now().UTC().Format(time.RFC3339Nano) + `","price":"1.0"}}`))
	}))
	defer srv.Close()

	const runFor = 2 * time.Second
	rep := runProbe(srv.URL, "", []endpoint{{Name: "price-tip", Path: "/price/tip"}},
		runFor, 2, slaTargets{P95MS: 5000, P99MS: 5000, FreshnessSec: 30, AvailabilityPct: 99.0})

	if len(rep.PerEndpoint) != 1 {
		t.Fatalf("PerEndpoint len=%d want 1", len(rep.PerEndpoint))
	}
	st := rep.PerEndpoint[0]
	if st.ObservedAtFreshSec == nil {
		t.Fatal("no freshness recorded — the fake API always returns observed_at")
	}
	// Ceiling sits between the true value (sub-millisecond) and the
	// end-of-run bias the fix removes (runFor/2 = 1.0 s).
	const ceiling = 0.25
	if *st.ObservedAtFreshSec > ceiling {
		t.Errorf("stalest freshness = %.3fs, want <= %.3fs — freshness is being charged "+
			"the distance to the end of the %v run instead of each sample's own instant",
			*st.ObservedAtFreshSec, ceiling, runFor)
	}
	if *st.ObservedAtFreshSec < 0 {
		t.Errorf("stalest freshness = %.3fs, want >= 0", *st.ObservedAtFreshSec)
	}
}

// TestAggregateEndpointStats_FreshnessUsesSampleReceiptInstant pins the
// exact corrected value with no wall-clock dependency: 121 samples over
// a 120 s run, every one of them read exactly 2 s after its own
// observed_at. The stalest freshness IS 2 s. Charging each sample to the
// end of the run instead yields ~122 s for the oldest sample.
func TestAggregateEndpointStats_FreshnessUsesSampleReceiptInstant(t *testing.T) {
	runStart := time.Now().Add(-2 * time.Minute)
	const observedAge = 2 * time.Second

	ss := make([]probeSample, 0, 121)
	for i := 0; i <= 120; i++ {
		receivedAt := runStart.Add(time.Duration(i) * time.Second)
		ss = append(ss, probeSample{
			latency:    5 * time.Millisecond,
			ok:         true,
			observedAt: receivedAt.Add(-observedAge),
			receivedAt: receivedAt,
		})
	}

	st := aggregateEndpointStats(endpoint{Name: "price-tip", Path: "/price/tip"}, ss)
	if st.ObservedAtFreshSec == nil {
		t.Fatal("ObservedAtFreshSec is nil, want the stalest sample")
	}
	if got, want := *st.ObservedAtFreshSec, observedAge.Seconds(); abs(got-want) > 1e-9 {
		t.Errorf("stalest freshness = %.9fs, want %.9fs", got, want)
	}
}

// TestAggregateEndpointStats_LatencyExcludesFailedSamples pins GH-740 (1):
// a failed request has no response latency, and a connection-refused one
// "takes" ~0 ms, so pooling failures into the percentiles drags them
// toward zero exactly when the API is down.
func TestAggregateEndpointStats_LatencyExcludesFailedSamples(t *testing.T) {
	var ss []probeSample
	for i := 0; i < 5; i++ {
		ss = append(ss,
			probeSample{latency: 100 * time.Millisecond, ok: true},
			probeSample{latency: 0, ok: false})
	}
	st := aggregateEndpointStats(endpoint{Name: "price", Path: "/price"}, ss)
	if st.LatencyMS.P50 != 100 || st.LatencyMS.P95 != 100 || st.LatencyMS.P99 != 100 {
		t.Errorf("latency p50/p95/p99 = %g/%g/%g ms, want 100/100/100 — the five "+
			"failed samples must not enter the percentiles",
			st.LatencyMS.P50, st.LatencyMS.P95, st.LatencyMS.P99)
	}
	if st.Samples != 10 || st.Errors != 5 || st.AvailabilityPct != 50 {
		t.Errorf("samples=%d errors=%d availability=%g, want 10/5/50",
			st.Samples, st.Errors, st.AvailabilityPct)
	}
}

// TestRunProbe_HardOutageEmitsNoLatency is GH-740 (1) end to end: a run
// against a refused port must not publish a 0 ms p95 that p95_breach and
// the weekly proof's worst-run p95 read as a very fast API.
func TestRunProbe_HardOutageEmitsNoLatency(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	deadURL := srv.URL
	srv.Close()

	rep := runProbe(deadURL, "", []endpoint{{Name: "price", Path: "/price"}},
		100*time.Millisecond, 1, slaTargets{P95MS: 200, P99MS: 500, FreshnessSec: 30, AvailabilityPct: 99.9})
	if rep.Verdict == "pass" {
		t.Fatal("verdict=pass against a refused port")
	}
	if st := rep.PerEndpoint[0]; st.Samples == 0 || st.Successes != 0 {
		t.Fatalf("samples=%d successes=%d, want >0 failed samples", st.Samples, st.Successes)
	}
	var buf strings.Builder
	if err := writeTextfile(&buf, &rep); err != nil {
		t.Fatalf("writeTextfile: %v", err)
	}
	if strings.Contains(buf.String(), `stellarindex_sla_probe_latency_ms{endpoint="price"`) {
		t.Errorf("an endpoint with no successful response published a latency:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), `stellarindex_sla_probe_availability_pct{endpoint="price"} 0.000`) {
		t.Errorf("availability for the refused endpoint must still read 0:\n%s", buf.String())
	}
}

// TestRunProbe_RequestHangingAtDeadlineIsAFailure pins GH-740 (2): the
// API answers three requests, then accepts the fourth and never replies.
// The run deadline must stop new requests, not cancel and discard the
// hanging one; otherwise every run straddling a hang reads 100 % and
// every later run records zero samples.
func TestRunProbe_RequestHangingAtDeadlineIsAFailure(t *testing.T) {
	var served atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if served.Add(1) <= 3 {
			_, _ = w.Write([]byte(`{}`))
			return
		}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)

	rep := runProbe(srv.URL, "", []endpoint{{Name: "price", Path: "/price"}},
		300*time.Millisecond, 1, slaTargets{P95MS: 5000, P99MS: 5000, FreshnessSec: 30, AvailabilityPct: 99.9})
	st := rep.PerEndpoint[0]
	// Errors may be 2: a second hung request can start if the first one's
	// client timer fires a tick before the run deadline's.
	if st.Successes != 3 || st.Errors < 1 || st.Errors > 2 || st.AvailabilityPct >= 100 {
		t.Fatalf("samples=%d successes=%d errors=%d, want 3 successes and the hang as 1-2 errors — the hanging request "+
			"must be recorded as a failure when its own timeout expires", st.Samples, st.Successes, st.Errors)
	}
	if rep.Verdict == "pass" {
		t.Error("verdict=pass while the API hung")
	}
}

// TestAggregateEndpointStats_FreshnessIsStalestResponse pins GH-743 (1):
// the 30 s freshness promise is per response, so a run where 49 % of
// reads are 120 s stale is a breach. A per-run median reads it as 1 s.
func TestAggregateEndpointStats_FreshnessIsStalestResponse(t *testing.T) {
	now := time.Now()
	var ss []probeSample
	for i := 0; i < 100; i++ {
		age := time.Second
		if i%2 == 0 && i < 98 {
			age = 120 * time.Second
		}
		ss = append(ss, probeSample{
			latency: 5 * time.Millisecond, ok: true,
			observedAt: now.Add(-age), receivedAt: now,
		})
	}
	st := aggregateEndpointStats(endpoint{Name: "price-tip", Path: "/price/tip"}, ss)
	if st.ObservedAtFreshSec == nil {
		t.Fatal("ObservedAtFreshSec is nil")
	}
	if got := *st.ObservedAtFreshSec; abs(got-120) > 1e-9 {
		t.Errorf("freshness = %.3fs, want 120s — 49 of 100 responses were 120 s stale", got)
	}
	if f := endpointFailures(st, slaTargets{P95MS: 200, P99MS: 500, FreshnessSec: 30, AvailabilityPct: 99.9}); len(f) == 0 {
		t.Error("no SLA failure for a run with 49 % of reads 4x over the 30 s promise")
	}
}

// TestRunProbe_PairEndpointsRejectBodiesThatBreakTheirContract pins
// GH-743 (2): a 2xx whose body cannot carry the measurement is not a
// success. A price surface without a parseable observed_at would
// otherwise drop the freshness series (and its alert) with no signal,
// and /oracle/latest answering `{"data":[]}` during an oracle outage
// would read 100 % available.
func TestRunProbe_PairEndpointsRejectBodiesThatBreakTheirContract(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/oracle/latest") {
			_, _ = w.Write([]byte(`{"data":[]}`))
			return
		}
		// A refactor nested observed_at one level down.
		_, _ = w.Write([]byte(`{"data":{"price":"1.0","meta":{"observed_at":"` +
			time.Now().UTC().Format(time.RFC3339) + `"}}}`))
	}))
	defer srv.Close()

	eps := pairEndpoints("native", "fiat:USD", defaultClosedBucketFreshTarget)
	rep := runProbe(srv.URL, "", eps, 150*time.Millisecond, 1, slaTargets{
		P95MS: 5000, P99MS: 5000, FreshnessSec: 30, AvailabilityPct: 99.9,
	})
	if rep.Verdict == "pass" {
		t.Fatalf("verdict=pass with every pair endpoint breaking its body contract")
	}
	for _, st := range rep.PerEndpoint {
		if st.Samples == 0 {
			t.Fatalf("%s: no samples", st.Endpoint)
		}
		if st.AvailabilityPct != 0 {
			t.Errorf("%s: availability=%g, want 0 — a 2xx that cannot carry the measurement is not a success",
				st.Endpoint, st.AvailabilityPct)
		}
	}
}

// TestHelperProcessMain runs main() with SLA_PROBE_HELPER_ARGS when
// re-executed by TestMain_RejectsOutOfRangeNumericFlags; skipped otherwise.
func TestHelperProcessMain(t *testing.T) {
	if os.Getenv("SLA_PROBE_HELPER") != "1" {
		t.Skip("helper process for TestMain_RejectsOutOfRangeNumericFlags")
	}
	os.Args = append([]string{"stellarindex-sla-probe"}, strings.Fields(os.Getenv("SLA_PROBE_HELPER_ARGS"))...)
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	main()
	os.Exit(0)
}

// TestMain_RejectsOutOfRangeNumericFlags pins GH-878: every numeric flag
// is range-checked before a request is made. Unchecked, each value below
// produced a complete, plausible report instead of a usage error — an
// expired -duration read as a total outage, a 0 availability target
// passed anything, a negative latency target failed everything.
func TestMain_RejectsOutOfRangeNumericFlags(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	deadURL := srv.URL
	srv.Close()

	cases := []struct{ name, args, flagName string }{
		{"zero duration", "-duration 0s", "-duration"},
		{"negative duration", "-duration -1s", "-duration"},
		{"zero p95", "-p95-target 0s", "-p95-target"},
		{"negative p99", "-p99-target -5ms", "-p99-target"},
		{"zero freshness", "-freshness-target 0s", "-freshness-target"},
		{"negative closed-bucket freshness", "-closed-bucket-freshness-target -1s", "-closed-bucket-freshness-target"},
		{"zero availability", "-availability-target 0", "-availability-target"},
		{"availability over 100", "-availability-target 100.5", "-availability-target"},
		{"NaN availability", "-availability-target NaN", "-availability-target"},
		{"zero concurrency", "-concurrency 0", "-concurrency"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// -duration 50ms keeps an unvalidated run short; a later
			// -duration in tc.args overrides it.
			cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcessMain$")
			cmd.Env = append(os.Environ(), "SLA_PROBE_HELPER=1",
				"SLA_PROBE_HELPER_ARGS=-base-url "+deadURL+" -duration 50ms "+tc.args)
			var stderr strings.Builder
			cmd.Stderr = &stderr
			err := cmd.Run()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
				t.Fatalf("%s: exit = %v, want usage exit 2; stderr:\n%s", tc.args, err, stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.flagName) {
				t.Errorf("%s: stderr does not name %s:\n%s", tc.args, tc.flagName, stderr.String())
			}
		})
	}
}

// TestHit_OracleWithReadingsIsASuccess keeps the contract check from
// rejecting the healthy shape it guards.
func TestHit_OracleWithReadingsIsASuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"source":"reflector","price":"0.1"}]}`))
	}))
	defer srv.Close()
	eps := pairEndpoints("native", "fiat:USD", defaultClosedBucketFreshTarget)
	oracle := eps[len(eps)-1]
	if oracle.Name != "oracle-latest" {
		t.Fatalf("last pair endpoint = %q, want oracle-latest", oracle.Name)
	}
	if _, ok, _ := hit(context.Background(), &http.Client{Timeout: time.Second}, srv.URL, "", oracle); !ok {
		t.Error("an oracle response with one reading was rejected")
	}
}

// TestRunProbe_MultiPairDoesNotMergeSamples is the regression guard for
// CA2-A31-harden-2: with two -pair flags, both pairs' endpoints share the
// bare names "price"/"price-tip"/"oracle-latest". Before the fix, samples
// were keyed by ep.Name alone, so native's fresh price-tip samples and
// USDC's 600s-stale ones landed in the same bucket and every PerEndpoint
// row for a given name was fed that merged slice — the stale pair's
// freshness became invisible (or was reported as a duplicate row
// indistinguishable from the fresh pair's).
//
// PROVEN-RED: keying samples[ep.Name] (main.go collectSamples/runProbe)
// instead of samples[sampleKey(ep)] makes this fail: both pairs'
// ObservedAtFreshSec collapse to the same merged value and the USDC pair
// is never named in FailedReasons.
func TestRunProbe_MultiPairDoesNotMergeSamples(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var observedAt time.Time
		if r.URL.Query().Get("asset") == "usdc" {
			// 600s stale — well past any 30s freshness target.
			observedAt = time.Now().UTC().Add(-600 * time.Second)
		} else {
			observedAt = time.Now().UTC()
		}
		_, _ = w.Write([]byte(`{"data":{"observed_at":"` + observedAt.Format(time.RFC3339Nano) + `","price":"1.0"}}`))
	}))
	defer srv.Close()

	var endpoints []endpoint
	endpoints = append(endpoints, pairEndpoints("native", "fiat:USD", defaultClosedBucketFreshTarget)...)
	endpoints = append(endpoints, pairEndpoints("usdc", "fiat:USD", defaultClosedBucketFreshTarget)...)

	// concurrency == len(endpoints): each worker starts on a distinct
	// endpoint (collectSamples' round-robin), guaranteeing every one of
	// the 6 endpoints gets sampled at least once within the short run.
	rep := runProbe(srv.URL, "", endpoints, 500*time.Millisecond, len(endpoints),
		slaTargets{P95MS: 5000, P99MS: 5000, FreshnessSec: 30, AvailabilityPct: 99.0})

	if len(rep.PerEndpoint) != 6 {
		t.Fatalf("PerEndpoint len=%d want 6 (2 pairs x 3 endpoints)", len(rep.PerEndpoint))
	}

	var nativeTip, usdcTip *stats
	for i := range rep.PerEndpoint {
		st := rep.PerEndpoint[i]
		if st.Endpoint != "price-tip" {
			continue
		}
		switch st.Pair {
		case "native/fiat:USD":
			nativeTip = &rep.PerEndpoint[i]
		case "usdc/fiat:USD":
			usdcTip = &rep.PerEndpoint[i]
		}
	}
	if nativeTip == nil || usdcTip == nil {
		t.Fatalf("missing per-pair price-tip row: native=%v usdc=%v", nativeTip, usdcTip)
	}
	if nativeTip.ObservedAtFreshSec == nil || usdcTip.ObservedAtFreshSec == nil {
		t.Fatalf("missing freshness samples: native=%v usdc=%v", nativeTip.ObservedAtFreshSec, usdcTip.ObservedAtFreshSec)
	}
	// The defect merges both pairs' samples into one bucket, so both
	// rows would read the same (merged) freshness. Asserting they
	// differ — and that each is close to its OWN pair's true value —
	// is what a merge cannot satisfy.
	const freshCeiling = 5.0
	if *nativeTip.ObservedAtFreshSec > freshCeiling {
		t.Errorf("native/fiat:USD price-tip freshness = %.1fs, want <= %.1fs (contaminated by the stale USDC pair)",
			*nativeTip.ObservedAtFreshSec, freshCeiling)
	}
	const staleFloor = 500.0
	if *usdcTip.ObservedAtFreshSec < staleFloor {
		t.Errorf("usdc/fiat:USD price-tip freshness = %.1fs, want >= %.1fs (hidden behind the fresh native pair)",
			*usdcTip.ObservedAtFreshSec, staleFloor)
	}

	foundUSDCFailure := false
	for _, r := range rep.FailedReasons {
		if strings.Contains(r, "price-tip[usdc/fiat:USD]") && strings.Contains(r, "freshness") {
			foundUSDCFailure = true
		}
	}
	if !foundUSDCFailure {
		t.Errorf("FailedReasons does not name the stale usdc pair's price-tip: %v", rep.FailedReasons)
	}
}

// TestMain_UsageDoesNotPrintAPIKey pins CA2-A31-harden-1: a flag-parse
// error prints Usage and the healthchecks wrapper uploads that output to a
// third party, so the env-supplied key must never render as a flag default.
func TestMain_UsageDoesNotPrintAPIKey(t *testing.T) {
	const probeKey = "fake-probe-key-not-a-real-value" // gitleaks:allow
	cases := []struct {
		args     string
		wantExit int
	}{
		{"-duration 120", 2}, // unitless duration: the wrapper's likeliest typo
		{"-h", 0},
	}
	for _, tc := range cases {
		args := tc.args
		t.Run(args, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcessMain$")
			cmd.Env = append(os.Environ(), "SLA_PROBE_HELPER=1",
				"STELLARINDEX_PROBE_API_KEY="+probeKey,
				"SLA_PROBE_HELPER_ARGS="+args)
			out, err := cmd.CombinedOutput()
			if got := cmd.ProcessState.ExitCode(); got != tc.wantExit {
				t.Fatalf("%s: exit = %d (%v), want %d; output:\n%s", args, got, err, tc.wantExit, out)
			}
			if !strings.Contains(string(out), "-api-key") {
				t.Fatalf("%s: output is not the usage text:\n%s", args, out)
			}
			if strings.Contains(string(out), probeKey) {
				t.Errorf("%s: usage output leaks STELLARINDEX_PROBE_API_KEY:\n%s", args, out)
			}
		})
	}
}

// TestResolveAPIKey_FallsBackToEnv keeps the wrapper's env-inheritance
// contract: with no -api-key on argv the probe still sends the env key.
func TestResolveAPIKey_FallsBackToEnv(t *testing.T) {
	t.Setenv("STELLARINDEX_PROBE_API_KEY", "env-value")
	if got := resolveAPIKey(""); got != "env-value" {
		t.Errorf(`resolveAPIKey("") = %q, want the env value`, got)
	}
	if got := resolveAPIKey("flag-value"); got != "flag-value" {
		t.Errorf("resolveAPIKey(flag) = %q, want the flag value", got)
	}
	t.Setenv("STELLARINDEX_PROBE_API_KEY", "")
	if got := resolveAPIKey(""); got != "" {
		t.Errorf("resolveAPIKey with no env or flag = %q, want empty", got)
	}
}
