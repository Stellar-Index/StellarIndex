package archive

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestRunOneTick_Agreement is the steady-state path: every region
// returns the same price for the same bucket. After one tick the
// "ok" outcome counter increments and the "divergence" counter
// stays at zero — the alert ratio in production is "rate(divergences)
// > 0", and we need this case not to trip it.
func TestRunOneTick_Agreement(t *testing.T) {
	resp := func() crossRegionResponse {
		return crossRegionResponse{
			From:  time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC),
			To:    time.Date(2026, 4, 27, 12, 0, 30, 0, time.UTC),
			Price: "0.1234567890",
		}
	}
	s1 := stubResponse(t, resp())
	defer s1.Close()
	s2 := stubResponse(t, resp())
	defer s2.Close()

	regions := []regionEndpoint{
		{name: "r1", base: s1.URL},
		{name: "r2", base: s2.URL},
	}

	reg := prometheus.NewRegistry()
	exp := newCrossRegionExporter(reg)
	client := &http.Client{Timeout: 5 * time.Second}

	runOneTick(t.Context(), client, regions, []string{"native/fiat:USD"}, metricVWAP,
		30*time.Second, 1, exp)

	if got := testutil.ToFloat64(exp.divergences.WithLabelValues("native/fiat:USD", "vwap")); got != 0 {
		t.Errorf("divergences counter = %v; want 0 on agreement", got)
	}
	if got := testutil.ToFloat64(exp.checksTotal.WithLabelValues("native/fiat:USD", "vwap", "ok")); got != 1 {
		t.Errorf("checks_total{outcome=ok} = %v; want 1", got)
	}
	if exp.lastRunUnix.Load() == 0 {
		t.Error("lastRunUnix should be set after a sweep")
	}
}

// TestRunOneTick_Divergence is the alert path: r2 returns a different
// price. The divergences counter increments, the checks_total under
// outcome=divergence increments, and outcome=ok stays at zero. Spot-
// checks the per-pair label so the alert can be filtered by pair.
func TestRunOneTick_Divergence(t *testing.T) {
	from := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	to := from.Add(30 * time.Second)
	s1 := stubResponse(t, crossRegionResponse{From: from, To: to, Price: "1.0000"})
	defer s1.Close()
	s2 := stubResponse(t, crossRegionResponse{From: from, To: to, Price: "1.0001"})
	defer s2.Close()

	regions := []regionEndpoint{
		{name: "r1", base: s1.URL},
		{name: "r2", base: s2.URL},
	}

	reg := prometheus.NewRegistry()
	exp := newCrossRegionExporter(reg)
	runOneTick(t.Context(), &http.Client{Timeout: 5 * time.Second},
		regions, []string{"crypto:BTC/fiat:USD"}, metricVWAP,
		30*time.Second, 1, exp)

	if got := testutil.ToFloat64(exp.divergences.WithLabelValues("crypto:BTC/fiat:USD", "vwap")); got != 1 {
		t.Errorf("divergences counter = %v; want 1 after divergence tick", got)
	}
	if got := testutil.ToFloat64(exp.checksTotal.WithLabelValues("crypto:BTC/fiat:USD", "vwap", "divergence")); got != 1 {
		t.Errorf("checks_total{outcome=divergence} = %v; want 1", got)
	}
	if got := testutil.ToFloat64(exp.checksTotal.WithLabelValues("crypto:BTC/fiat:USD", "vwap", "ok")); got != 0 {
		t.Errorf("checks_total{outcome=ok} should stay 0 on divergence, got %v", got)
	}
}

// TestRunOneTick_FetchErrorTracked covers the partial-failure path:
// one region's HTTP fetch fails, the other succeeds. The fetch_errors
// counter increments for the failed region only — divergences stay
// at zero (we don't flag divergence based on a fetch failure, that
// would conflate "region down" with "regions disagree").
func TestRunOneTick_FetchErrorTracked(t *testing.T) {
	from := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	to := from.Add(30 * time.Second)
	s1 := stubResponse(t, crossRegionResponse{From: from, To: to, Price: "1.0"})
	defer s1.Close()
	// s2 always 500s.
	s2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	defer s2.Close()

	regions := []regionEndpoint{
		{name: "r1", base: s1.URL},
		{name: "r2", base: s2.URL},
	}

	reg := prometheus.NewRegistry()
	exp := newCrossRegionExporter(reg)
	runOneTick(t.Context(), &http.Client{Timeout: 5 * time.Second},
		regions, []string{"native/fiat:USD"}, metricVWAP,
		30*time.Second, 1, exp)

	if got := testutil.ToFloat64(exp.fetchErrors.WithLabelValues("r2", "native/fiat:USD", "vwap")); got != 1 {
		t.Errorf("fetch_errors{region=r2} = %v; want 1", got)
	}
	if got := testutil.ToFloat64(exp.fetchErrors.WithLabelValues("r1", "native/fiat:USD", "vwap")); got != 0 {
		t.Errorf("fetch_errors{region=r1} = %v; want 0 (r1 was healthy)", got)
	}
	if got := testutil.ToFloat64(exp.divergences.WithLabelValues("native/fiat:USD", "vwap")); got != 0 {
		t.Errorf("divergences = %v; partial fetch failure must not flag divergence", got)
	}
}

// TestAllFailed_Outcome confirms the "outcome=error" counter only
// increments when every region failed — distinguishes "the whole
// monitoring host can't reach anything" from "regions agree on data".
func TestAllFailed_Outcome(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer s.Close()
	regions := []regionEndpoint{
		{name: "r1", base: s.URL},
		{name: "r2", base: s.URL},
	}

	reg := prometheus.NewRegistry()
	exp := newCrossRegionExporter(reg)
	runOneTick(t.Context(), &http.Client{Timeout: 5 * time.Second},
		regions, []string{"native/fiat:USD"}, metricVWAP,
		30*time.Second, 1, exp)

	if got := testutil.ToFloat64(exp.checksTotal.WithLabelValues("native/fiat:USD", "vwap", "error")); got != 1 {
		t.Errorf("checks_total{outcome=error} = %v; want 1 when all regions failed", got)
	}
}

// ─── C4-007: /healthz must reflect the LAST tick, not the first ────
//
// The pre-fix verdict was `lastRunUnix != 0` — a latch the very first
// sweep set and nothing ever cleared, so the monitor reported healthy
// forever afterwards no matter what happened to the tick loop or to the
// regions. Each case below is a state in which the old handler returned
// 200 and the corrected one must not.

// TestCrossRegionHealth_StaleTick — the loop stopped ticking (goroutine
// wedged/dead, or resolveAnchor failing every time so lastRunUnix is
// never re-stored). The last sweep is older than the staleness bound.
func TestCrossRegionHealth_StaleTick(t *testing.T) {
	reg := prometheus.NewRegistry()
	exp := newCrossRegionExporter(reg)
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	staleAfter := 190 * time.Second // healthStaleAfter(60s, 10s)

	// A sweep that completed AND reached regions — but ten minutes ago.
	exp.lastRunUnix.Store(now.Add(-10 * time.Minute).Unix())
	exp.lastReachedUnix.Store(now.Add(-10 * time.Minute).Unix())

	code, msg := crossRegionHealth(exp, now, staleAfter)
	if code != http.StatusServiceUnavailable {
		t.Errorf("code = %d, want %d for a sweep 10m stale (bound %s)",
			code, http.StatusServiceUnavailable, staleAfter)
	}
	if !strings.Contains(msg, "tick loop is stuck or dead") {
		t.Errorf("msg = %q, want it to name the stuck tick loop", msg)
	}
}

// TestCrossRegionHealth_EveryRegionFailing — the loop is ticking fine,
// but every region fetch has failed since startup. runOneTick still
// stamps lastRunUnix on such a sweep (the sweep DID complete), which is
// exactly why the old latch stayed green while the monitor was blind.
func TestCrossRegionHealth_EveryRegionFailing(t *testing.T) {
	reg := prometheus.NewRegistry()
	exp := newCrossRegionExporter(reg)
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	staleAfter := 190 * time.Second

	exp.lastRunUnix.Store(now.Add(-5 * time.Second).Unix()) // fresh sweep
	// lastReachedUnix left at 0: no sweep ever got data back.

	code, msg := crossRegionHealth(exp, now, staleAfter)
	if code != http.StatusServiceUnavailable {
		t.Errorf("code = %d, want %d when no sweep has ever reached a region",
			code, http.StatusServiceUnavailable)
	}
	if !strings.Contains(msg, "every region fetch is failing") {
		t.Errorf("msg = %q, want it to name the failing fetches", msg)
	}
}

// TestCrossRegionHealth_RegionsWentAwayAfterAGoodStart is the C4-007
// scenario in its most literal form: the first sweep succeeded (so the
// old latch was set for good), then every region went away and stayed
// away. The loop keeps ticking, so lastRunUnix stays fresh; only
// lastReachedUnix goes stale.
func TestCrossRegionHealth_RegionsWentAwayAfterAGoodStart(t *testing.T) {
	reg := prometheus.NewRegistry()
	exp := newCrossRegionExporter(reg)
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	staleAfter := 190 * time.Second

	exp.lastRunUnix.Store(now.Add(-5 * time.Second).Unix())   // ticking
	exp.lastReachedUnix.Store(now.Add(-1 * time.Hour).Unix()) // blind for an hour

	code, msg := crossRegionHealth(exp, now, staleAfter)
	if code != http.StatusServiceUnavailable {
		t.Errorf("code = %d, want %d — ticking but blind for an hour is not healthy",
			code, http.StatusServiceUnavailable)
	}
	if !strings.Contains(msg, "every region fetch is failing") {
		t.Errorf("msg = %q, want it to name the failing fetches", msg)
	}
}

// TestCrossRegionHealth_Healthy pins the positive case so the fix can't
// be "return 503 always": a recent sweep that reached regions is 200.
func TestCrossRegionHealth_Healthy(t *testing.T) {
	reg := prometheus.NewRegistry()
	exp := newCrossRegionExporter(reg)
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	staleAfter := 190 * time.Second

	exp.lastRunUnix.Store(now.Add(-30 * time.Second).Unix())
	exp.lastReachedUnix.Store(now.Add(-30 * time.Second).Unix())

	if code, msg := crossRegionHealth(exp, now, staleAfter); code != http.StatusOK || msg != "ok" {
		t.Errorf("crossRegionHealth = (%d, %q), want (200, \"ok\") for a 30s-old good sweep", code, msg)
	}
}

// TestCrossRegionHealth_BeforeFirstSweep keeps the one behaviour the old
// handler got right: 503 until something has actually run.
func TestCrossRegionHealth_BeforeFirstSweep(t *testing.T) {
	reg := prometheus.NewRegistry()
	exp := newCrossRegionExporter(reg)
	code, msg := crossRegionHealth(exp, time.Now(), 190*time.Second)
	if code != http.StatusServiceUnavailable {
		t.Errorf("code = %d, want %d before the first sweep", code, http.StatusServiceUnavailable)
	}
	if !strings.Contains(msg, "no check sweep has completed yet") {
		t.Errorf("msg = %q, want the pre-first-sweep message", msg)
	}
}

// TestRunOneTick_AllFailedDoesNotStampReached is the end-to-end half:
// a real sweep against two dead regions must NOT advance the
// reachability clock, so the health verdict above can go red. Pre-fix
// there was no such clock — runOneTick stamped lastRunUnix and the
// handler called that healthy.
func TestRunOneTick_AllFailedDoesNotStampReached(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer s.Close()
	regions := []regionEndpoint{
		{name: "r1", base: s.URL},
		{name: "r2", base: s.URL},
	}

	reg := prometheus.NewRegistry()
	exp := newCrossRegionExporter(reg)
	runOneTick(t.Context(), &http.Client{Timeout: 5 * time.Second},
		regions, []string{"native/fiat:USD"}, metricVWAP,
		30*time.Second, 1, exp)

	if exp.lastRunUnix.Load() == 0 {
		t.Fatal("lastRunUnix must still be stamped — the sweep did complete")
	}
	if got := exp.lastReachedUnix.Load(); got != 0 {
		t.Errorf("lastReachedUnix = %d, want 0 — no region answered", got)
	}
	if code, _ := crossRegionHealth(exp, time.Now(), 190*time.Second); code != http.StatusServiceUnavailable {
		t.Errorf("health after an all-regions-down sweep = %d, want %d",
			code, http.StatusServiceUnavailable)
	}
}

// TestRunOneTick_ReachedStampedOnPartialFailure — one region up, one
// down still counts as "reached": the fetch-error counter carries the
// partial failure, and flipping /healthz red for it would page on a
// single region's blip. Pins the boundary crossRegionHealth documents.
func TestRunOneTick_ReachedStampedOnPartialFailure(t *testing.T) {
	from := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	to := from.Add(30 * time.Second)
	s1 := stubResponse(t, crossRegionResponse{From: from, To: to, Price: "1.0"})
	defer s1.Close()
	s2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	defer s2.Close()

	reg := prometheus.NewRegistry()
	exp := newCrossRegionExporter(reg)
	runOneTick(t.Context(), &http.Client{Timeout: 5 * time.Second},
		[]regionEndpoint{{name: "r1", base: s1.URL}, {name: "r2", base: s2.URL}},
		[]string{"native/fiat:USD"}, metricVWAP, 30*time.Second, 1, exp)

	if exp.lastReachedUnix.Load() == 0 {
		t.Error("lastReachedUnix = 0; a partial failure still reached r1")
	}
	if code, _ := crossRegionHealth(exp, time.Now(), 190*time.Second); code != http.StatusOK {
		t.Errorf("health after a partial failure = %d, want 200", code)
	}
}

// TestHealthStaleAfter pins the threshold derivation: three missed ticks
// plus one full per-region timeout. A regression to a fixed constant
// would silently mis-tune every non-default -interval.
func TestHealthStaleAfter(t *testing.T) {
	if got, want := healthStaleAfter(60*time.Second, 10*time.Second), 190*time.Second; got != want {
		t.Errorf("healthStaleAfter(60s, 10s) = %s, want %s", got, want)
	}
	if got, want := healthStaleAfter(5*time.Minute, 30*time.Second), 15*time.Minute+30*time.Second; got != want {
		t.Errorf("healthStaleAfter(5m, 30s) = %s, want %s", got, want)
	}
}

// TestAllFailed_Helper directly exercises the helper used by
// runOneTick to flag the "everyone is unreachable" outcome.
func TestAllFailed_Helper(t *testing.T) {
	cases := []struct {
		name string
		in   []regionResult
		want bool
	}{
		{"empty", nil, false},
		{"all-ok", []regionResult{{Err: nil}, {Err: nil}}, false},
		{"one-failed", []regionResult{{Err: nil}, {Err: fmt.Errorf("boom")}}, false},
		{"all-failed", []regionResult{{Err: fmt.Errorf("a")}, {Err: fmt.Errorf("b")}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := allFailed(tc.in); got != tc.want {
				t.Errorf("allFailed(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// stubResponse returns an httptest.Server that always returns the
// supplied response wrapped in {"data": ...}. Mirrors stubServer in
// cross_region_check_test.go but local to this file so we don't
// depend on test-helper visibility across files.
func stubResponse(t *testing.T, body crossRegionResponse) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": body})
	}))
}

// TestRunOneTick_PartialFailureIsNotLabelledOK is the regression test
// for the cold audit of 2026-08-04.
//
// runOneTick discarded analyseRegionResults' `compared` return and
// defaulted outcome to "ok", so a sweep in which fewer than two regions
// responded — i.e. one where NOTHING was compared — emitted the same
// time series as a genuine agreement. In a two-region fleet that means
// one region being down produces a permanent green: divergences flat,
// last_reached fresh, /healthz 200, and zero comparisons performed.
// analyseRegionResults documents that such a sample "proves NOTHING"
// (OBS-07), and the one-shot sibling exits 1 on it.
//
// The existing TestRunOneTick_FetchErrorTracked builds this exact
// scenario and asserts fetchErrors and divergences — but never the
// outcome label, which is why the defect was invisible.
func TestRunOneTick_PartialFailureIsNotLabelledOK(t *testing.T) {
	from := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	to := from.Add(30 * time.Second)
	s1 := stubResponse(t, crossRegionResponse{From: from, To: to, Price: "1.0"})
	defer s1.Close()
	s2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	defer s2.Close()

	reg := prometheus.NewRegistry()
	exp := newCrossRegionExporter(reg)
	runOneTick(t.Context(), &http.Client{Timeout: 5 * time.Second},
		[]regionEndpoint{{name: "r1", base: s1.URL}, {name: "r2", base: s2.URL}},
		[]string{"native/fiat:USD"}, metricVWAP, 30*time.Second, 1, exp)

	okCount := testutil.ToFloat64(
		exp.checksTotal.WithLabelValues("native/fiat:USD", metricVWAP.String(), "ok"))
	if okCount != 0 {
		t.Errorf("checks_total{outcome=\"ok\"} = %v, want 0 — only one region responded, so nothing was compared; labelling that \"ok\" is indistinguishable from a real agreement", okCount)
	}
	inconclusive := testutil.ToFloat64(
		exp.checksTotal.WithLabelValues("native/fiat:USD", metricVWAP.String(), "inconclusive"))
	if inconclusive != 1 {
		t.Errorf("checks_total{outcome=\"inconclusive\"} = %v, want 1", inconclusive)
	}
}
