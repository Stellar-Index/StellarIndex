package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
