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

// TestRunOneTick_Agreement is the steady state: matching prices bump only the "ok" counter, so the rate(divergences) > 0 alert stays quiet.
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

// TestRunOneTick_Divergence is the alert path: a differing price bumps divergences and checks_total{outcome=divergence}, labelled per pair.
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

// TestRunOneTick_FetchErrorTracked: a failed region fetch bumps fetch_errors only, never divergences ("region down" is not "regions disagree").
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

// TestAllFailed_Outcome: outcome=error increments only when every region failed.
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

// /healthz must reflect the LAST tick, not the first. A latch on lastRunUnix != 0 stays healthy forever; each case below is a state where such a handler returns 200 and the real one must not.

// TestCrossRegionHealth_StaleTick: the loop stopped ticking, so the last sweep is older than the staleness bound.
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

// TestCrossRegionHealth_EveryRegionFailing: ticks complete (so lastRunUnix is stamped) but every fetch failed; health must still go red.
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

// TestCrossRegionHealth_RegionsWentAwayAfterAGoodStart: the loop ticks but every region vanished, so only lastReachedUnix goes stale.
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

// TestCrossRegionHealth_Healthy: a recent sweep that reached regions is 200, so the fix cannot be "always 503".
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

// TestCrossRegionHealth_BeforeFirstSweep: 503 until something has run.
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

// TestRunOneTick_AllFailedDoesNotStampReached: a sweep against two dead regions must not advance the reachability clock.
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

// TestRunOneTick_ReachedStampedOnPartialFailure: one region up still counts as reached; fetch_errors carries the failure and /healthz must not page on one blip.
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

// TestHealthStaleAfter pins the threshold: three missed ticks plus one per-region timeout, so non-default -interval values stay tuned.
func TestHealthStaleAfter(t *testing.T) {
	if got, want := healthStaleAfter(60*time.Second, 10*time.Second), 190*time.Second; got != want {
		t.Errorf("healthStaleAfter(60s, 10s) = %s, want %s", got, want)
	}
	if got, want := healthStaleAfter(5*time.Minute, 30*time.Second), 15*time.Minute+30*time.Second; got != want {
		t.Errorf("healthStaleAfter(5m, 30s) = %s, want %s", got, want)
	}
}

// TestAllFailed_Helper exercises the helper runOneTick uses to flag "everyone is unreachable".
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

// TestRunOneTick_PartialFailureIsNotLabelledOK: a sweep where fewer than two regions responded compared nothing and must not carry the "ok" label.
// Otherwise one region down in a two-region fleet is a permanent green: analyseRegionResults says such a sample proves NOTHING.
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
