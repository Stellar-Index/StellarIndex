package obs_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

func TestHandler_ExposesMetrics(t *testing.T) {
	// Warm up every registered Vec with at least one child so the
	// scrape output includes its HELP/TYPE header. Prometheus
	// CounterVec/GaugeVec without children don't appear in scrapes
	// (by design — there's nothing to show).
	obs.HTTPRequestsTotal.WithLabelValues("GET", "/_warmup", "200").Inc()
	obs.HTTPRequestDuration.WithLabelValues("GET", "/_warmup").Observe(0.001)
	obs.SourceEventsTotal.WithLabelValues("_warmup").Inc()
	obs.SourceLastEventUnix.WithLabelValues("_warmup").Set(0)
	obs.SourceEnabled.WithLabelValues("_warmup").Set(0)
	obs.SourceDecodeErrorsTotal.WithLabelValues("_warmup").Inc()
	obs.SourceOrphanEventsTotal.WithLabelValues("_warmup").Inc()
	obs.SourceInsertErrorsTotal.WithLabelValues("_warmup", "trade").Inc()
	obs.RateLimitFailOpenTotal.Inc()
	obs.Sep1CacheOpsTotal.WithLabelValues("hit").Inc()
	obs.CursorLastLedger.WithLabelValues("_warmup").Set(0)
	obs.PriceStalenessSeconds.WithLabelValues("_warmup").Set(0)
	obs.OracleLastUpdateUnix.WithLabelValues("_warmup", "_warmup").Set(0)
	obs.OracleResolutionSeconds.WithLabelValues("_warmup").Set(0)
	obs.AggregatorTicksTotal.WithLabelValues("_warmup").Inc()
	obs.AggregatorVWAPWritesTotal.Inc()
	obs.AggregatorEmptyWindowsTotal.Inc()
	obs.AggregatorDroppedTradesTotal.WithLabelValues("_warmup", "_warmup").Inc()

	ts := httptest.NewServer(obs.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	text := string(body)

	expected := []string{
		"http_requests_total",
		"http_request_duration_seconds",
		"stellarindex_source_events_total",
		"stellarindex_source_last_event_unix",
		"stellarindex_source_enabled",
		"stellarindex_source_decode_errors_total",
		"stellarindex_source_orphan_events_total",
		"stellarindex_source_insert_errors_total",
		"stellarindex_ratelimit_fail_open_total",
		"stellarindex_sep1_cache_ops_total",
		"stellarindex_cursor_last_ledger",
		"stellarindex_price_staleness_seconds",
		"stellarindex_oracle_last_update_unix",
		"stellarindex_oracle_resolution_seconds",
		"stellarindex_aggregator_ticks_total",
		"stellarindex_aggregator_vwap_writes_total",
		"stellarindex_aggregator_empty_windows_total",
		"stellarindex_aggregator_dropped_trades_total",
		"stellarindex_usage_rollup_sweeps_total",
		// v0.21.4 background cache workers — the counters are seeded at
		// register time, so they must be present in a fresh scrape.
		"stellarindex_dex_tvl_refresh_total",
		"stellarindex_sdex_orderbook_maintain_total",
		"stellarindex_explorer_swr_refresh_total",
		// Language-native + process metrics from collectors.
		"go_goroutines",
		"process_open_fds",
	}
	for _, metric := range expected {
		if !strings.Contains(text, metric) {
			t.Errorf("metric %q missing from scrape output", metric)
		}
	}
}

func TestHTTPMetrics_CountsRequests(t *testing.T) {
	// Use a fresh sub-mux to avoid polluting counters across tests.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /foo", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /bar", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	h := obs.HTTPMetrics(mux)

	// Hit /foo twice, /bar once.
	for i := 0; i < 2; i++ {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/foo", nil))
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/bar", nil))

	// Scrape the registry + look for the counts.
	ts := httptest.NewServer(obs.Handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	text := string(body)

	// Assert both counter rows landed. Exact format:
	//   http_requests_total{method="GET",route="/foo",status="200"} 2
	//   http_requests_total{method="GET",route="/bar",status="500"} 1
	for _, want := range []string{
		`http_requests_total{method="GET",route="/foo",status="200"} 2`,
		`http_requests_total{method="GET",route="/bar",status="500"} 1`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("scrape missing %q; body:\n%s", want, text)
		}
	}
}

func TestHTTPMetrics_LowercaseMethodIsCanonicalised(t *testing.T) {
	// An attacker sending "get" instead of "GET" would otherwise
	// double the method-label cardinality. Middleware uppercases
	// known methods before stamping the label.
	//
	// Handler catches everything with no pattern so the method label
	// is the only axis varying across requests.
	h := obs.HTTPMetrics(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for _, verb := range []string{"get", "GeT", "GET"} {
		r := httptest.NewRequest(verb, "/anything", nil)
		r.Method = verb // httptest.NewRequest uppercases — override.
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)
	}

	ts := httptest.NewServer(obs.Handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	text := string(body)

	// All three requests must collapse into the same method="GET"
	// series — i.e. the counter should reach 3.
	want := `method="GET"`
	if !strings.Contains(text, want) {
		t.Errorf("canonical method label missing; expected %q in scrape", want)
	}
	// Crucially: "get" and "GeT" variants MUST NOT appear
	// separately — that would signal the cardinality leak.
	for _, bad := range []string{`method="get"`, `method="GeT"`} {
		if strings.Contains(text, bad) {
			t.Errorf("non-canonical method leaked into labels: %q", bad)
		}
	}
}

func TestHTTPMetrics_ClientAbortLabelled499(t *testing.T) {
	// A handler that never calls WriteHeader combined with a
	// ctx-cancelled request simulates the "client hung up before
	// we wrote anything" case. Without the 499 label it'd record
	// as 200 (statusRecorder default).
	h := obs.HTTPMetrics(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Don't write anything; rely on ctx cancellation to
		// indicate client abort.
		<-r.Context().Done()
	}))

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // immediately cancelled
	r := httptest.NewRequest(http.MethodGet, "/v1/slow-op", nil).WithContext(ctx)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	ts := httptest.NewServer(obs.Handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if !strings.Contains(string(body), `status="499"`) {
		t.Errorf("expected status=\"499\" for aborted request, got:\n%s", string(body))
	}
}

func TestHTTPMetrics_UnmatchedRouteLabelled(t *testing.T) {
	// Hit a path with no pattern registered — middleware labels it
	// "unmatched" to prevent cardinality blow-up.
	mux := http.NewServeMux()
	// No routes registered; every request is a 404 with empty pattern.

	h := obs.HTTPMetrics(mux)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/does-not-exist", nil))

	ts := httptest.NewServer(obs.Handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if !strings.Contains(string(body), `route="unmatched"`) {
		t.Errorf("expected route=\"unmatched\" label, got:\n%s", string(body))
	}
}

// TestHTTPMetrics_RouteSurvivesWithContextChain pins the regression
// behind /v1/status's broken SLO burn-rate alerts: an inner
// middleware that calls r.WithContext(...) creates a new
// http.Request struct, and ServeMux sets Pattern on that struct —
// not the one HTTPMetrics holds. Without obs.CaptureRoute wired
// innermost, every request labels as "unmatched" in production.
func TestHTTPMetrics_RouteSurvivesWithContextChain(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/probe", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Inner middleware that mimics Logger's r.WithContext shadowing.
	withContextMW := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := context.WithValue(r.Context(), struct{ k string }{"req_id"}, "abc")
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}

	// Production-equivalent stack: HTTPMetrics → withContextMW →
	// CaptureRoute → mux. CaptureRoute is innermost.
	h := obs.HTTPMetrics(withContextMW(obs.CaptureRoute(mux)))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/probe", nil))

	ts := httptest.NewServer(obs.Handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	text := string(body)

	// Positive assertion is sufficient: if the route was lost, this
	// counter would be route="unmatched" instead.
	want := `http_requests_total{method="GET",route="/v1/probe",status="200"} 1`
	if !strings.Contains(text, want) {
		t.Errorf("expected %q, body:\n%s", want, text)
	}
}

// TestHTTPMetrics_SyntheticUASkipsHistogram pins the SLO-noise
// fix: requests with `User-Agent: stellarindex-smoke/N` (the smoke
// timer) MUST NOT contribute to the http_requests_total counter
// or http_request_duration_seconds histogram. Otherwise every
// smoke fire pollutes the SLO recording rule with cold-cache
// samples customers never see.
func TestHTTPMetrics_SyntheticUASkipsHistogram(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/synthetic-probe", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := obs.HTTPMetrics(mux)

	// Real customer request — should land in the histogram.
	rr := httptest.NewRecorder()
	customerReq := httptest.NewRequest(http.MethodGet, "/v1/synthetic-probe", nil)
	customerReq.Header.Set("User-Agent", "Mozilla/5.0")
	h.ServeHTTP(rr, customerReq)

	// Synthetic probe — should be skipped.
	rr2 := httptest.NewRecorder()
	smokeReq := httptest.NewRequest(http.MethodGet, "/v1/synthetic-probe", nil)
	smokeReq.Header.Set("User-Agent", "stellarindex-smoke/1")
	h.ServeHTTP(rr2, smokeReq)

	ts := httptest.NewServer(obs.Handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	// Exactly 1 sample for /v1/synthetic-probe — the customer
	// request. The smoke request should be invisible.
	want := `http_requests_total{method="GET",route="/v1/synthetic-probe",status="200"} 1`
	if !strings.Contains(string(body), want) {
		t.Errorf("expected %q, body:\n%s", want, string(body))
	}
	// Defensive: if the smoke request had been counted, the value
	// would be 2. Pin the negative.
	if strings.Contains(string(body), `http_requests_total{method="GET",route="/v1/synthetic-probe",status="200"} 2`) {
		t.Errorf("regression: smoke traffic counted in customer histogram")
	}
}

// TestHTTPMetrics_StreamRouteSkipsDurationHistogram pins the
// latency-SLO fix: an SSE / streaming route (pattern ending in
// /stream) MUST still be counted in http_requests_total but kept
// OUT of the http_request_duration_seconds histogram. The handler
// returns only when the client disconnects, so its "duration" is
// the connection lifetime — feeding that to the latency histogram
// pins p99 at the +Inf bucket and burns the latency SLO.
func TestHTTPMetrics_StreamRouteSkipsDurationHistogram(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/streamtest/stream", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := obs.HTTPMetrics(mux)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/streamtest/stream", nil)
	req.Header.Set("User-Agent", "Mozilla/5.0")
	h.ServeHTTP(rr, req)

	ts := httptest.NewServer(obs.Handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	s := string(body)

	// The request IS counted in http_requests_total.
	if !strings.Contains(s, `http_requests_total{method="GET",route="/v1/streamtest/stream",status="200"} 1`) {
		t.Errorf("stream request not counted in http_requests_total; body:\n%s", s)
	}
	// The duration is NOT observed — no histogram series for the route.
	if strings.Contains(s, `http_request_duration_seconds_count{method="GET",route="/v1/streamtest/stream"}`) {
		t.Errorf("regression: stream route landed in the latency histogram")
	}
}

// TestZeroSeed_F0033 verifies that the bounded counters whose alert
// rules reference rate()/increase() have all their label combos
// pre-registered at zero, so PromQL queries return 0 rather than
// "no data" before the first event fires. This was F-0033 in the
// 2026-05-26 audit: operators saw the alert reference but no series
// in /metrics output and couldn't tell whether the metric was
// "intentionally zero" or "alert references a dead metric."
func TestZeroSeed_F0033(t *testing.T) {
	ts := httptest.NewServer(obs.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	s := string(body)

	mustContain := []string{
		`stellarindex_aggregator_triangulations_total{outcome="ok"} 0`,
		`stellarindex_aggregator_triangulations_total{outcome="missing_leg"} 0`,
		`stellarindex_aggregator_triangulations_total{outcome="parse_error"} 0`,
		`stellarindex_aggregator_triangulations_total{outcome="redis_error"} 0`,
		// frozen_leg (MNY-22) shipped after the original four and was
		// not seeded with them. It is the outcome that matters most to
		// an operator — "a chain refused to publish because one of its
		// legs was frozen" — and until it first fired, the series was
		// absent, so a `rate(...{outcome="frozen_leg"}[15m])` panel read
		// as "no data" (indistinguishable from a dead metric) rather
		// than as a healthy zero.
		`stellarindex_aggregator_triangulations_total{outcome="frozen_leg"} 0`,
		// The self-pair exploit detector is by-design flat-at-zero; without
		// this seed an operator can't distinguish "armed" from "dead metric."
		`stellarindex_amm_self_pair_swap_total{source="comet"} 0`,
		`stellarindex_usage_rollup_sweeps_total{outcome="ok"}`,
		`stellarindex_usage_rollup_sweeps_total{outcome="scan_error"} 0`,
		`stellarindex_usage_rollup_sweeps_total{outcome="sink_error"} 0`,
		// ADR-0019 freeze lifecycle. The escalation counter drives a
		// severity:page rule, so "no data" vs "zero" is the difference
		// between an operator believing the P1 is armed and it being
		// dead. Unlabelled counters/gauges seed on registration; the
		// released_total label set is seeded explicitly.
		`stellarindex_anomaly_freeze_escalated_total 0`,
		`stellarindex_anomaly_freeze_extensions_total 0`,
		`stellarindex_anomaly_freeze_active 0`,
		`stellarindex_anomaly_freeze_released_total{mode="auto"} 0`,
		`stellarindex_anomaly_freeze_released_total{mode="operator"} 0`,
		// C3-082 / C3-067 (audit-2026-07-23). Both are fail-open /
		// best-effort paths whose whole point is that they are silent when
		// healthy, so "absent" and "zero" have to be distinguishable or the
		// alert is indistinguishable from a dead metric — which is exactly
		// how both of these went unobserved until the audit found them.
		`stellarindex_monthly_quota_fail_open_total 0`,
		// W1-flow-register-4: the dwell-guarded fail-CLOSED twin. Its whole
		// point is to be silent until a SUSTAINED counter outage forces a
		// 429, so — like its fail-open sibling — "absent" and "zero" must be
		// distinguishable or the alert is indistinguishable from a dead metric.
		`stellarindex_monthly_quota_fail_closed_total 0`,
		`stellarindex_admin_audit_write_failures_total{surface="account_override"} 0`,
		`stellarindex_admin_audit_write_failures_total{surface="key_mint"} 0`,
		`stellarindex_admin_audit_write_failures_total{surface="key_revoke"} 0`,
		`stellarindex_admin_audit_write_failures_total{surface="status_notice"} 0`,
		// C3-056: the staff PII look-up joined the set. It is a read, not
		// a mutation, which is exactly why its audit row is the ONLY
		// evidence it happened — an absent series here would make "no
		// staff look-up has ever lost its row" indistinguishable from
		// "the look-up never got wired to the audit sink at all", which
		// is the pre-fix state.
		`stellarindex_admin_audit_write_failures_total{surface="staff_customer_lookup"} 0`,
		// Tier clamp: not alerted, but `failed` means paid throughput
		// stayed live past a downgrade and must not read as "no data".
		`stellarindex_admin_key_budget_clamps_total{outcome="lowered"} 0`,
		`stellarindex_admin_key_budget_clamps_total{outcome="failed"} 0`,
		// Migration 0119: a counter whose entire job is to make a SILENT
		// failure visible must not itself be absent until it first fires.
		`stellarindex_anomaly_freeze_ladder_write_failures_total{op="mark_hold"} 0`,
		`stellarindex_anomaly_freeze_ladder_write_failures_total{op="clear"} 0`,
		`stellarindex_anomaly_freeze_ladder_rehydrated_total 0`,
		// C3-032: all three login-code-lockout failure paths are silent
		// at the HTTP layer by design (the control is defence-in-depth
		// over the per-token cap, so it fails soft). `status_check` in
		// particular means the lockout is not being enforced AT ALL while
		// the response looks healthy — an absent series there is the
		// difference between an operator seeing a disabled control and
		// assuming the metric is dead.
		`stellarindex_login_code_lockout_errors_total{op="status_check"} 0`,
		`stellarindex_login_code_lockout_errors_total{op="register"} 0`,
		`stellarindex_login_code_lockout_errors_total{op="sweep"} 0`,
		// The table's key is attacker-chosen and the endpoint that writes
		// it is unauthenticated, so the row count is a security signal,
		// not capacity trivia. Gauges register at zero; asserted here so
		// a future refactor cannot drop it from the registry list.
		`stellarindex_login_code_lockout_rows 0`,
		`stellarindex_login_code_lockout_rows_deleted_total 0`,
		// Notify (Resend) sends (task #33 / W8 recon 9c). A mail outage is
		// otherwise silent (the login handler swallows the send error), so
		// the failure-ratio alert must read a real 0 before the first email
		// — an absent series would make "no mail has ever failed" and "the
		// mailer is dead" indistinguishable.
		`stellarindex_notify_sends_total{result="sent",template="magic-link"} 0`,
		`stellarindex_notify_sends_total{result="failed",template="magic-link"} 0`,
		`stellarindex_notify_sends_total{result="sent",template="signup-verify"} 0`,
		`stellarindex_notify_sends_total{result="failed",template="signup-verify"} 0`,
	}
	for _, want := range mustContain {
		if !strings.Contains(s, want) {
			t.Errorf("scrape body missing pre-seeded series: %q", want)
		}
	}
}
