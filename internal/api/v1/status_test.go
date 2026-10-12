package v1

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// fakeStatusBackend is a hand-rolled StatusBackend that lets tests
// stub each signal independently. Mirrors the four backend methods
// without touching Prometheus.
type fakeStatusBackend struct {
	heartbeats map[string]time.Time
	latency    StatusLatency
	freshness  StatusFreshness
	incidents  StatusIncidents

	sourceEntries24h map[string]int64

	hbErr, latErr, freErr, incErr, entriesErr error
	sourceEnabled                             map[string]bool
	enabledErr                                error
}

func (f *fakeStatusBackend) Heartbeats(context.Context) (map[string]time.Time, error) {
	return f.heartbeats, f.hbErr
}

func (f *fakeStatusBackend) Latency(context.Context) (StatusLatency, error) {
	return f.latency, f.latErr
}

func (f *fakeStatusBackend) Freshness(context.Context) (StatusFreshness, error) {
	return f.freshness, f.freErr
}

func (f *fakeStatusBackend) Incidents(context.Context) (StatusIncidents, error) {
	return f.incidents, f.incErr
}

func (f *fakeStatusBackend) SourceEntries24h(context.Context) (map[string]int64, error) {
	return f.sourceEntries24h, f.entriesErr
}

func (f *fakeStatusBackend) SourceEnabled(context.Context) (map[string]bool, error) {
	return f.sourceEnabled, f.enabledErr
}

// getStatus serves GET /v1/status and returns the raw recorder, the envelope
// and the typed payload.
func getStatus(t *testing.T, opts Options) (*httptest.ResponseRecorder, Envelope, StatusResponse) {
	t.Helper()
	rr := httptest.NewRecorder()
	New(opts).Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/status", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200", rr.Code)
	}
	var env Envelope
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	data, _ := json.Marshal(env.Data)
	var st StatusResponse
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatalf("decode StatusResponse: %v", err)
	}
	return rr, env, st
}

func TestStatus_NoBackend_DegradedSurface(t *testing.T) {
	rr, env, st := getStatus(t, Options{
		RegionName:       "r1",
		RegionDeployment: "production",
	})

	if st.Region.Name != "r1" {
		t.Errorf("Region.Name = %q, want r1", st.Region.Name)
	}
	// api=ok with indexer/aggregator unknown is partial visibility and must
	// roll up to "degraded", not "ok".
	if st.Overall != "degraded" {
		t.Errorf("Overall = %q, want degraded (partial visibility: api ok, indexer/aggregator unknown)", st.Overall)
	}
	if !env.Flags.Stale {
		t.Errorf("flags.stale = false; want true when no backend wired")
	}

	want := map[string]string{"api": "ok", "indexer": "unknown", "aggregator": "unknown"}
	got := map[string]string{}
	for _, s := range st.Services {
		got[s.Name] = s.Status
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("services[%q] = %q, want %q", k, got[k], v)
		}
	}

	body := rr.Body.String()
	if !strings.Contains(body, `"freshness_status":"unknown"`) {
		t.Errorf("no-backend body lacks freshness_status unknown:\n%s", body)
	}
	if strings.Contains(body, `"active_sources"`) {
		t.Errorf("no-backend body carries active_sources:\n%s", body)
	}
}

func TestStatus_WithBackend_HappyPath(t *testing.T) {
	now := time.Now().UTC()
	_, _, st := getStatus(t, Options{
		RegionName: "r1",
		StatusBackend: &fakeStatusBackend{
			heartbeats: map[string]time.Time{
				"indexer":    now.Add(-5 * time.Second),
				"aggregator": now.Add(-3 * time.Second),
			},
			latency: StatusLatency{P50Ms: 10, P95Ms: 80, P99Ms: 200, WindowSecs: 300},
			freshness: StatusFreshness{
				LastAggregatorTick: WireTime(now),
				ActiveSources:      new(14),
				TotalSources:       new(18),
			},
			incidents: StatusIncidents{ActiveCount: 0},
		},
	})

	if st.Overall != "ok" {
		t.Errorf("Overall = %q, want ok", st.Overall)
	}
	got := map[string]string{}
	for _, s := range st.Services {
		got[s.Name] = s.Status
	}
	for _, n := range []string{"api", "indexer", "aggregator"} {
		if got[n] != "ok" {
			t.Errorf("services[%q] = %q, want ok", n, got[n])
		}
	}
	if st.Latency.P99Ms != 200 {
		t.Errorf("Latency.P99Ms = %v, want 200", st.Latency.P99Ms)
	}
	if st.Freshness.ActiveSources == nil || *st.Freshness.ActiveSources != 14 {
		t.Errorf("Freshness.ActiveSources = %v, want 14", st.Freshness.ActiveSources)
	}
	// 14/18 is a shortfall: flagged on the freshness block, not in overall.
	if st.FreshnessStatus != "degraded" {
		t.Errorf("FreshnessStatus = %q, want degraded", st.FreshnessStatus)
	}
}

// A served {0,17} is a real, alarming reading and must reach the wire as
// active_sources:0; a failed freshness query must omit both counts and
// report freshness_status "unknown" rather than a measured zero.
func TestStatus_FreshnessCounts_ServedZeroVsFailedQuery(t *testing.T) {
	cases := []struct {
		name       string
		backend    *fakeStatusBackend
		wantStatus string
		wantBody   []string
		absentBody []string
		// wantOverall is asserted when set; the failed-query case must
		// degrade overall like any blind panel.
		wantOverall string
	}{
		{
			name: "served zero",
			backend: &fakeStatusBackend{
				freshness: StatusFreshness{ActiveSources: new(0), TotalSources: new(17)},
			},
			wantStatus: "degraded",
			wantBody:   []string{`"active_sources":0`, `"total_sources":17`},
		},
		{
			name: "failed query",
			backend: &fakeStatusBackend{
				freshness: StatusFreshness{ActiveSources: new(0), TotalSources: new(17)},
				freErr:    errors.New("prometheus: connection refused"),
				heartbeats: map[string]time.Time{
					"indexer": time.Now().UTC(), "aggregator": time.Now().UTC(),
				},
			},
			wantStatus:  "unknown",
			absentBody:  []string{`"active_sources"`, `"total_sources"`},
			wantOverall: "degraded",
		},
		{
			name: "all active",
			backend: &fakeStatusBackend{
				freshness: StatusFreshness{ActiveSources: new(17), TotalSources: new(17)},
			},
			wantStatus: "ok",
			wantBody:   []string{`"active_sources":17`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := New(Options{RegionName: "r1", StatusBackend: tc.backend})
			rr := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/status", nil))
			body := rr.Body.String()

			var env struct {
				Data struct {
					FreshnessStatus string `json:"freshness_status"`
					Overall         string `json:"overall"`
				} `json:"data"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if env.Data.FreshnessStatus != tc.wantStatus {
				t.Errorf("freshness_status = %q, want %q", env.Data.FreshnessStatus, tc.wantStatus)
			}
			if tc.wantOverall != "" && env.Data.Overall != tc.wantOverall {
				t.Errorf("overall = %q, want %q", env.Data.Overall, tc.wantOverall)
			}
			for _, s := range tc.wantBody {
				if !strings.Contains(body, s) {
					t.Errorf("body missing %s:\n%s", s, body)
				}
			}
			for _, s := range tc.absentBody {
				if strings.Contains(body, s) {
					t.Errorf("body carries %s on a failed query:\n%s", s, body)
				}
			}
		})
	}
}

// A failed Alertmanager query must not serialise as an all-clear: it must carry incidents_status="unknown" with
// counts left at zero (not invented), not read as "0 active alerts" while alerting is blind. Raw wire JSON is
// decoded to pin what a browser receives.
func TestStatus_IncidentsQueryError_ReportsUnknownNotZero(t *testing.T) {
	srv := New(Options{
		RegionName: "r1",
		StatusBackend: &fakeStatusBackend{
			// Every other signal is healthy; only the incidents query
			// fails, isolating the incidents-block honesty from the
			// overall-rollup degradation (which is covered separately).
			heartbeats: map[string]time.Time{
				"indexer":    time.Now().UTC(),
				"aggregator": time.Now().UTC(),
			},
			incErr: errors.New("prometheus: connection refused"),
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200", rr.Code)
	}

	// Assert against the raw wire bytes, not the typed struct, so the
	// field is proven present and correct on the JSON a client sees.
	var env struct {
		Data struct {
			IncidentsStatus string `json:"incidents_status"`
			Incidents       struct {
				ActiveCount int `json:"active_count"`
			} `json:"incidents"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode body: %v", err)
	}

	if env.Data.IncidentsStatus != "unknown" {
		t.Errorf("incidents_status = %q, want \"unknown\" on a failed Alertmanager query (must not read as all-clear)", env.Data.IncidentsStatus)
	}
	if env.Data.Incidents.ActiveCount != 0 {
		t.Errorf("active_count = %d, want 0 (counts must not be invented on error)", env.Data.Incidents.ActiveCount)
	}
	// The whole point: the wire must not present a green all-clear.
	if strings.Contains(rr.Body.String(), `"incidents_status":"ok"`) {
		t.Errorf("body reports incidents_status=ok during a query failure — all-clear derived from a failure:\n%s", rr.Body.String())
	}
}

// TestPrometheusStatusBackend_FreshnessReturnsQueryError pins that every
// freshness query's failure reaches the caller, and that an empty count()
// vector is a measured zero rather than a failure.
func TestPrometheusStatusBackend_FreshnessReturnsQueryError(t *testing.T) {
	const empty = `{"status":"success","data":{"resultType":"vector","result":[]}}`
	for _, failing := range []string{activeSourcesQuery, totalSourcesQuery, "vwap_writes_total"} {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Query().Get("query"), failing) {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte(empty))
		}))
		p := &PrometheusStatusBackend{URL: ts.URL}
		got, err := p.Freshness(context.Background())
		ts.Close()
		if err == nil {
			t.Errorf("query %q failed but Freshness returned nil error (%+v)", failing, got)
		}
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(empty))
	}))
	defer ts.Close()
	got, err := (&PrometheusStatusBackend{URL: ts.URL}).Freshness(context.Background())
	if err != nil {
		t.Fatalf("Freshness on empty vectors: %v", err)
	}
	if got.ActiveSources == nil || *got.ActiveSources != 0 || got.TotalSources == nil || *got.TotalSources != 0 {
		t.Errorf("empty count() = %v / %v, want measured 0 / 0", got.ActiveSources, got.TotalSources)
	}
}

func TestPrometheusStatusBackend_QueryShape(t *testing.T) {
	// Hand-rolled HTTP server returning a canned Prometheus
	// instant-query response. Verifies the client parses it
	// correctly without hitting a real Prometheus.
	const body = `{
		"status":"success",
		"data":{
			"resultType":"vector",
			"result":[
				{"metric":{"job":"stellarindex-indexer"},"value":[1730000000,"1730000050"]},
				{"metric":{"job":"stellarindex-aggregator"},"value":[1730000000,"1730000048"]}
			]
		}
	}`
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/v1/query") {
			t.Errorf("path = %q, want /api/v1/query*", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer ts.Close()

	p := &PrometheusStatusBackend{URL: ts.URL}
	hb, err := p.Heartbeats(context.Background())
	if err != nil {
		t.Fatalf("Heartbeats: %v", err)
	}
	if len(hb) != 2 {
		t.Fatalf("hb len = %d, want 2", len(hb))
	}
	if _, ok := hb["indexer"]; !ok {
		t.Errorf("hb[\"indexer\"] missing")
	}
	if _, ok := hb["aggregator"]; !ok {
		t.Errorf("hb[\"aggregator\"] missing")
	}
}

// TestPrometheusStatusBackend_HeartbeatQueryIgnoresFailedScrapes pins the PromQL shape: a FAILED
// scrape still writes an up=0 sample with a fresh timestamp, so timestamp(up{...}) without a value
// filter never goes stale for a crashed target. The query must filter on `== 1` first.
func TestPrometheusStatusBackend_HeartbeatQueryIgnoresFailedScrapes(t *testing.T) {
	var gotQuery string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("query")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer ts.Close()

	p := &PrometheusStatusBackend{URL: ts.URL}
	if _, err := p.Heartbeats(context.Background()); err != nil {
		t.Fatalf("Heartbeats: %v", err)
	}

	if !strings.Contains(gotQuery, "== 1") {
		t.Errorf("heartbeat query = %q, want it to filter on up==1 so a failing scrape (up=0) does not refresh the heartbeat timestamp", gotQuery)
	}
}

func TestPrometheusStatusBackend_IncidentsParsesAlertsAndCounts(t *testing.T) {
	// Three firing alerts: 1 page, 1 ticket, 1 informational, plus
	// the deadmansswitch which the query excludes server-side. The
	// client tally should match the labels.
	const body = `{
		"status":"success",
		"data":{
			"resultType":"vector",
			"result":[
				{"metric":{"alertname":"stellarindex_api_down","alertstate":"firing","severity":"page"},"value":[1730000000,"1"]},
				{"metric":{"alertname":"stellarindex_aggregator_silent","alertstate":"firing","severity":"ticket"},"value":[1730000000,"1"]},
				{"metric":{"alertname":"stellarindex_host_cpu_high","alertstate":"firing","severity":"informational"},"value":[1730000000,"1"]}
			]
		}
	}`
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer ts.Close()

	p := &PrometheusStatusBackend{URL: ts.URL}
	got, err := p.Incidents(context.Background())
	if err != nil {
		t.Fatalf("Incidents: %v", err)
	}
	if got.PageCount != 1 || got.TicketCount != 1 || got.InformationalCount != 1 {
		t.Errorf("counts = %+v, want page=1 ticket=1 info=1", got)
	}
	if got.ActiveCount != 3 {
		t.Errorf("ActiveCount = %d, want 3", got.ActiveCount)
	}
	if len(got.Active) != 3 {
		t.Fatalf("Active len = %d, want 3", len(got.Active))
	}
	// page first, then ticket, then informational.
	wantOrder := []string{
		"stellarindex_api_down",
		"stellarindex_aggregator_silent",
		"stellarindex_host_cpu_high",
	}
	for i, want := range wantOrder {
		if got.Active[i].Name != want {
			t.Errorf("Active[%d] = %q, want %q", i, got.Active[i].Name, want)
		}
	}
}

func TestPrometheusStatusBackend_IncidentsDedupesByAlertname(t *testing.T) {
	// Two label-sets for the same alertname (per-instance fan-out)
	// — the public surface dedupes by name.
	const body = `{
		"status":"success",
		"data":{
			"resultType":"vector",
			"result":[
				{"metric":{"alertname":"stellarindex_host_down","alertstate":"firing","severity":"ticket","instance":"r1"},"value":[1730000000,"1"]},
				{"metric":{"alertname":"stellarindex_host_down","alertstate":"firing","severity":"ticket","instance":"r2"},"value":[1730000000,"1"]}
			]
		}
	}`
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer ts.Close()

	p := &PrometheusStatusBackend{URL: ts.URL}
	got, err := p.Incidents(context.Background())
	if err != nil {
		t.Fatalf("Incidents: %v", err)
	}
	if got.ActiveCount != 1 {
		t.Errorf("ActiveCount = %d, want 1 (deduped)", got.ActiveCount)
	}
	if len(got.Active) != 1 {
		t.Errorf("Active len = %d, want 1", len(got.Active))
	}
}

// TestPrometheusStatusBackend_IncidentsNormalizesUnknownSeverity pins that the operator-controlled
// `severity` label, published on the public /v1/status JSON, is normalized to page/ticket/
// informational before it reaches ActiveIncident.
func TestPrometheusStatusBackend_IncidentsNormalizesUnknownSeverity(t *testing.T) {
	const body = `{
		"status":"success",
		"data":{
			"resultType":"vector",
			"result":[
				{"metric":{"alertname":"stellarindex_weird_alert","alertstate":"firing","severity":"<script>alert(1)</script>"},"value":[1730000000,"1"]}
			]
		}
	}`
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer ts.Close()

	p := &PrometheusStatusBackend{URL: ts.URL}
	got, err := p.Incidents(context.Background())
	if err != nil {
		t.Fatalf("Incidents: %v", err)
	}
	if got.PageCount != 0 || got.TicketCount != 0 || got.InformationalCount != 0 {
		t.Errorf("counts = %+v, want all zero for an out-of-enum severity", got)
	}
	if len(got.Active) != 1 {
		t.Fatalf("Active len = %d, want 1", len(got.Active))
	}
	if got.Active[0].Severity != "unknown" {
		t.Errorf("Active[0].Severity = %q, want %q (unvalidated label must not pass through)", got.Active[0].Severity, "unknown")
	}
}

// TestStatus_OverallRollup pins that `overall` is the worst per-service state plus the two
// cross-cutting canaries (backend-error, page-firing). Each row hits one branch of rollupOverall;
// flags.stale must be the inverse of overall=="ok".
func TestStatus_OverallRollup(t *testing.T) {
	now := time.Now().UTC()
	recent := now.Add(-3 * time.Second) // within 60s threshold
	stale := now.Add(-10 * time.Minute) // past 60s threshold

	type expect struct {
		overall string
		stale   bool
		indexer string // indexer service status, asserted when set
	}
	cases := []struct {
		name    string
		backend *fakeStatusBackend
		want    expect
	}{
		{
			// All three services healthy, no canary trips → ok.
			name: "all_ok",
			backend: &fakeStatusBackend{
				heartbeats: map[string]time.Time{
					"indexer": recent, "aggregator": recent,
				},
			},
			want: expect{overall: "ok", stale: false},
		},
		{
			// Indexer heartbeat way past threshold → svc=down → overall=down.
			name: "any_down",
			backend: &fakeStatusBackend{
				heartbeats: map[string]time.Time{
					"indexer": stale, "aggregator": recent,
				},
			},
			want: expect{overall: "down", stale: true, indexer: "down"},
		},
		{
			// Backend errors on every query and services degrade to unknown (hbErr != nil branch). api=ok keeps anyOK=true;
			// indexer/aggregator unknown gives mixed, so degraded (the backendErr canary also forces this).
			name: "backend_error_partial",
			backend: &fakeStatusBackend{
				hbErr:  errors.New("prometheus dead"),
				latErr: errors.New("prometheus dead"),
				freErr: errors.New("prometheus dead"),
				incErr: errors.New("prometheus dead"),
			},
			want: expect{overall: "degraded", stale: true},
		},
		{
			// No service is down/degraded, but a page-severity
			// alert is firing → cross-cutting canary trips
			// degraded.
			name: "page_alert_firing",
			backend: &fakeStatusBackend{
				heartbeats: map[string]time.Time{
					"indexer": recent, "aggregator": recent,
				},
				incidents: StatusIncidents{
					ActiveCount: 1, PageCount: 1,
					Active: []ActiveIncident{
						{Name: "stellarindex_api_down", Severity: "page"},
						{Name: "stellarindex_aggregator_silent", Severity: "ticket"},
					},
				},
			},
			want: expect{overall: "degraded", stale: true},
		},
		{
			// Mixed known/unknown (api ok + indexer/aggregator
			// unknown because heartbeats map is empty) → partial
			// visibility surfaces as degraded, NOT ok.
			name: "mixed_ok_and_unknown",
			backend: &fakeStatusBackend{
				heartbeats: map[string]time.Time{
					// neither indexer nor aggregator present
				},
			},
			want: expect{overall: "degraded", stale: true},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, env, st := getStatus(t, Options{RegionName: "r1", StatusBackend: tc.backend})

			if st.Overall != tc.want.overall {
				t.Errorf("Overall = %q, want %q", st.Overall, tc.want.overall)
			}
			for _, svc := range st.Services {
				if svc.Name == "indexer" && tc.want.indexer != "" && svc.Status != tc.want.indexer {
					t.Errorf("indexer.Status = %q, want %q", svc.Status, tc.want.indexer)
				}
			}
			if env.Flags.Stale != tc.want.stale {
				t.Errorf("flags.stale = %v, want %v", env.Flags.Stale, tc.want.stale)
			}
		})
	}
}

// TestRollupOverall_AllUnknownBranch exercises the all-services-unknown branch of rollupOverall
// directly, since the handler always stamps an "ok" api entry and HTTP tests cannot reach it. The
// "unknown" verdict is distinct from "down" and "ok".
func TestRollupOverall_AllUnknownBranch(t *testing.T) {
	// All three services unknown with no canary tripped: every signal is unknown with zero LastSeen, so overall must
	// roll to "unknown", not "ok".
	services := []StatusService{
		{Name: "api", Status: "unknown"},
		{Name: "indexer", Status: "unknown"},
		{Name: "aggregator", Status: "unknown"},
	}
	if got := rollupOverall(services, false, false, false); got != "unknown" {
		t.Errorf("all-unknown rollup = %q, want unknown", got)
	}

	// Zero-time LastSeen on a non-api "ok" service is treated as
	// unknown — guards against a backend returning Status="ok"
	// with no heartbeat data behind it.
	services = []StatusService{
		{Name: "indexer", Status: "ok"}, // zero LastSeen
	}
	if got := rollupOverall(services, false, false, false); got != "unknown" {
		t.Errorf("zero-time ok rollup = %q, want unknown", got)
	}

	// A service explicitly marked degraded → overall=degraded.
	services = []StatusService{
		{Name: "api", Status: "ok", LastSeen: WireTime(time.Now())},
		{Name: "indexer", Status: "degraded", LastSeen: WireTime(time.Now())},
	}
	if got := rollupOverall(services, false, false, false); got != "degraded" {
		t.Errorf("any-degraded rollup = %q, want degraded", got)
	}
}

// TestRollupOverallLatencyBreach guards that a green roll-up is impossible while the latency SLO
// the same response advertises is breached; the roll-up must not judge service liveness alone.
func TestRollupOverallLatencyBreach(t *testing.T) {
	t.Parallel()
	healthy := []StatusService{
		{Name: "api", Status: "ok", LastSeen: WireTime(time.Now())},
		{Name: "indexer", Status: "ok", LastSeen: WireTime(time.Now())},
		{Name: "aggregator", Status: "ok", LastSeen: WireTime(time.Now())},
	}
	if got := rollupOverall(healthy, false, false, false); got != "ok" {
		t.Fatalf("healthy + within-SLO rollup = %q, want ok", got)
	}
	if got := rollupOverall(healthy, false, false, true); got != "degraded" {
		t.Errorf("healthy services but breached latency SLO = %q, want degraded", got)
	}
}

// TestStatusLatencyBreached pins the threshold arithmetic, including no-data: WindowSecs==0 means nothing
// measurable and must not read as a breach (absence of data is handled by the unknown/backendErr paths).
func TestStatusLatencyBreached(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   StatusLatency
		want bool
	}{
		{"no data", StatusLatency{WindowSecs: 0, P95Ms: 9999, P99Ms: 9999}, false},
		{"within both", StatusLatency{WindowSecs: 300, P95Ms: 120, P99Ms: 400}, false},
		{"exactly at target", StatusLatency{WindowSecs: 300, P95Ms: 200, P99Ms: 500}, false},
		{"p95 over", StatusLatency{WindowSecs: 300, P95Ms: 201, P99Ms: 400}, true},
		{"p99 over", StatusLatency{WindowSecs: 300, P95Ms: 120, P99Ms: 501}, true},
		// The values actually observed in production.
		{"observed prod", StatusLatency{WindowSecs: 300, P95Ms: 840.5, P99Ms: 2096.4}, true},
	}
	for _, tc := range cases {
		if got := tc.in.breached(); got != tc.want {
			t.Errorf("%s: breached() = %t, want %t", tc.name, got, tc.want)
		}
	}
}

// TestStatus_TicketIncidentsDoNotMoveOverall pins rollupOverall precedence: `overall` escalates only on a service
// fault, metrics-backend error, breached latency SLO or `page`-severity alert. Ticket and informational alerts
// leave it "ok" (flags.stale false) however many fire; folding them in would change what "ok" promises publicly.
func TestStatus_TicketIncidentsDoNotMoveOverall(t *testing.T) {
	now := time.Now().UTC()
	recent := now.Add(-3 * time.Second) // within the 60s heartbeat threshold

	cases := []struct {
		name      string
		incidents StatusIncidents
		want      string
		// wantIncidentsStatus is the incidents_status tri-state, asserted when
		// set: a successful query reads "ok" or "degraded", never a hard-coded
		// "unknown".
		wantIncidentsStatus string
	}{
		{
			name:                "no alerts firing",
			incidents:           StatusIncidents{},
			want:                "ok",
			wantIncidentsStatus: "ok",
		},
		{
			name: "one ticket",
			incidents: StatusIncidents{
				ActiveCount: 1, TicketCount: 1,
				Active: []ActiveIncident{
					{Name: "stellarindex_source_stalled", Severity: "ticket"},
				},
			},
			want: "ok",
		},
		{
			name: "eight tickets — the reported payload",
			incidents: StatusIncidents{
				ActiveCount: 8, TicketCount: 8,
			},
			want: "ok",
		},
		{
			name: "thirty tickets and an informational — measured on r1",
			incidents: StatusIncidents{
				ActiveCount: 31, TicketCount: 30, InformationalCount: 1,
			},
			want: "ok",
		},
		{
			// Control: the severity that DOES escalate still does.
			name: "a page alongside the tickets",
			incidents: StatusIncidents{
				ActiveCount: 9, TicketCount: 8, PageCount: 1,
				Active: []ActiveIncident{
					{Name: "stellarindex_api_down", Severity: "page"},
				},
			},
			want:                "degraded",
			wantIncidentsStatus: "degraded",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, env, st := getStatus(t, Options{
				RegionName: "r1",
				StatusBackend: &fakeStatusBackend{
					heartbeats: map[string]time.Time{
						"indexer": recent, "aggregator": recent,
					},
					incidents: tc.incidents,
				},
			})
			if tc.wantIncidentsStatus != "" && st.IncidentsStatus != tc.wantIncidentsStatus {
				t.Errorf("incidents_status = %q, want %q", st.IncidentsStatus, tc.wantIncidentsStatus)
			}

			if st.Overall != tc.want {
				t.Errorf("Overall = %q, want %q", st.Overall, tc.want)
			}
			if wantStale := tc.want != "ok"; env.Flags.Stale != wantStale {
				t.Errorf("flags.stale = %v, want %v", env.Flags.Stale, wantStale)
			}
			// The counts the banner reads must still be on the wire
			// untouched — the note is rendered from them.
			if st.Incidents.ActiveCount != tc.incidents.ActiveCount {
				t.Errorf("incidents.active_count = %d, want %d",
					st.Incidents.ActiveCount, tc.incidents.ActiveCount)
			}
			if st.Incidents.PageCount != tc.incidents.PageCount {
				t.Errorf("incidents.page_count = %d, want %d", st.Incidents.PageCount, tc.incidents.PageCount)
			}
			if len(st.Incidents.Active) != len(tc.incidents.Active) {
				t.Fatalf("incidents.active len = %d, want %d", len(st.Incidents.Active), len(tc.incidents.Active))
			}
			for i, a := range tc.incidents.Active {
				if st.Incidents.Active[i].Name != a.Name {
					t.Errorf("incidents.active[%d] = %q, want %q", i, st.Incidents.Active[i].Name, a.Name)
				}
			}
		})
	}
}

// Lean test-net deployments run no aggregator (`run_aggregator: false`), a deployment shape not an outage. A
// hardcoded {"indexer","aggregator"} list would pin `overall` at "degraded"; with the deployment's own service
// list a healthy indexer alone rolls up to "ok" and the aggregator is not reported.
func TestStatus_DeclaredServicesOnly_AbsentAggregatorIsNotDegraded(t *testing.T) {
	now := time.Now().UTC()
	srv := New(Options{
		RegionName:     "testnet",
		StatusServices: []string{"indexer"},
		StatusBackend: &fakeStatusBackend{
			heartbeats: map[string]time.Time{
				"indexer": now.Add(-5 * time.Second),
			},
			latency:   StatusLatency{P50Ms: 10, P95Ms: 80, P99Ms: 200, WindowSecs: 300},
			freshness: StatusFreshness{ActiveSources: new(1), TotalSources: new(1)},
			incidents: StatusIncidents{ActiveCount: 0},
		},
	})

	st := decodeStatus(t, srv)
	got := statusByName(st)
	if _, reported := got["aggregator"]; reported {
		t.Errorf("services contains %q on a deployment that declares only [indexer]: %v",
			"aggregator", got)
	}
	if got["indexer"] != "ok" {
		t.Errorf("services[indexer] = %q, want ok", got["indexer"])
	}
	if st.Overall != "ok" {
		t.Errorf("Overall = %q, want ok — an aggregator this deployment "+
			"deliberately does not run must not roll up as degradation", st.Overall)
	}
}

// The opt-out must not become a blindfold: a declared service with a missing heartbeat is still
// partial visibility, so `overall` degrades. Fail-closed, or dropping a service from
// status_services would hide a real outage.
func TestStatus_DeclaredServiceWithNoHeartbeat_StillDegrades(t *testing.T) {
	srv := New(Options{
		RegionName:     "testnet",
		StatusServices: []string{"indexer"},
		StatusBackend: &fakeStatusBackend{
			heartbeats: map[string]time.Time{},
			latency:    StatusLatency{P50Ms: 10, P95Ms: 80, P99Ms: 200, WindowSecs: 300},
			incidents:  StatusIncidents{ActiveCount: 0},
		},
	})

	st := decodeStatus(t, srv)
	if got := statusByName(st); got["indexer"] != "unknown" {
		t.Errorf("services[indexer] = %q, want unknown", got["indexer"])
	}
	if st.Overall != "degraded" {
		t.Errorf("Overall = %q, want degraded — a DECLARED service with no "+
			"heartbeat is still partial visibility", st.Overall)
	}
}

// Pubnet's shape is unchanged: an Options with no StatusServices keeps
// reporting (and rolling up) both background services, including on the
// no-metrics-backend surface.
func TestStatus_UndeclaredServicesDefaultToPubnetPair(t *testing.T) {
	srv := New(Options{RegionName: "r1"})

	st := decodeStatus(t, srv)
	got := statusByName(st)
	for _, name := range []string{"indexer", "aggregator"} {
		if got[name] != "unknown" {
			t.Errorf("services[%q] = %q, want unknown — the default service "+
				"list must stay the pubnet pair", name, got[name])
		}
	}
}

// The deployment tier comes from config (an operator fact), not a literal: a hardcoded "production" made
// api.testnet.stellarindex.io answer `"deployment":"production"` and the explorer tag a test net PRODUCTION.
func TestStatus_DeploymentTierIsReportedVerbatim(t *testing.T) {
	srv := New(Options{RegionName: "testnet", RegionDeployment: "testnet"})

	st := decodeStatus(t, srv)
	if st.Region.Deployment != "testnet" {
		t.Errorf("Region.Deployment = %q, want testnet", st.Region.Deployment)
	}
}

// decodeStatus runs GET /v1/status against srv and unwraps the
// envelope. The three status tests below all need the same six lines.
func decodeStatus(t *testing.T, srv *Server) StatusResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200", rr.Code)
	}
	var env Envelope
	if err := json.NewDecoder(rr.Body).Decode(&env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	body, _ := json.Marshal(env.Data)
	var st StatusResponse
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatalf("re-decode status: %v", err)
	}
	return st
}

func statusByName(st StatusResponse) map[string]string {
	got := map[string]string{}
	for _, s := range st.Services {
		got[s.Name] = s.Status
	}
	return got
}

// `api.status_services` is validated case-insensitively, so statusServicesOr must lower-case as well as trim: the
// heartbeat map is keyed by lower-case Prometheus `job` labels minus the `stellarindex-` prefix. Otherwise
// `["Indexer", "Aggregator"]` passes validation but reports both "unknown" and `overall` stays degraded.

// TestStatusServicesOrNormalisesCase pins the boundary transform. It
// must match the one config validation applies, or a value can pass
// validation and still match nothing.
func TestStatusServicesOrNormalisesCase(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		in   []string
		want []string
	}{
		{"capitalised", []string{"Indexer", "Aggregator"}, []string{"indexer", "aggregator"}},
		{"upper", []string{"INDEXER"}, []string{"indexer"}},
		{"mixed with padding", []string{"  Indexer  "}, []string{"indexer"}},
		{"already canonical", []string{"indexer"}, []string{"indexer"}},
		{"blank entries dropped", []string{"Indexer", "   ", ""}, []string{"indexer"}},
		{"empty falls back to the default pair", nil, defaultStatusServices},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := statusServicesOr(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("statusServicesOr(%q) = %q, want %q", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("statusServicesOr(%q)[%d] = %q, want %q",
						tc.in, i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestCapitalisedStatusServiceResolvesItsHeartbeat is the end-to-end statement: a capitalised declaration must
// grade against the lower-cased heartbeat key Prometheus publishes, not sit at "unknown".
func TestCapitalisedStatusServiceResolvesItsHeartbeat(t *testing.T) {
	t.Parallel()
	// The shape PrometheusStatusBackend.Heartbeats returns: job labels
	// with the `stellarindex-` prefix stripped — always lower-case.
	hb := map[string]time.Time{
		"indexer":    time.Now().UTC(),
		"aggregator": time.Now().UTC(),
	}

	svcs := heartbeatServices(statusServicesOr([]string{"Indexer", "Aggregator"}), hb)
	if len(svcs) != 2 {
		t.Fatalf("got %d services, want 2", len(svcs))
	}
	for _, svc := range svcs {
		if svc.Status != "ok" {
			t.Errorf("service %q graded %q, want \"ok\" — a capitalised "+
				"status_services entry passed config validation and then "+
				"matched no heartbeat, pinning overall at degraded forever "+
				"(RD-05)", svc.Name, svc.Status)
		}
		if svc.LastSeen.IsZero() {
			t.Errorf("service %q has no last_seen; the heartbeat was not found", svc.Name)
		}
	}
}

// TestStatusServiceStaleHeartbeatStillGradesDown guards the blast
// radius: normalising the NAME must not disturb the freshness grading
// the list exists to drive.
func TestStatusServiceStaleHeartbeatStillGradesDown(t *testing.T) {
	t.Parallel()
	hb := map[string]time.Time{
		"indexer": time.Now().UTC().Add(-2 * statusHeartbeatStaleAfter),
	}
	svcs := heartbeatServices(statusServicesOr([]string{"Indexer"}), hb)
	if len(svcs) != 1 {
		t.Fatalf("got %d services, want 1", len(svcs))
	}
	if svcs[0].Status != "down" {
		t.Errorf("stale heartbeat graded %q, want \"down\"", svcs[0].Status)
	}
}

// The queries behind /v1/status `freshness.active_sources` and `freshness.total_sources` are
// evaluated by Prometheus, so no Go test can run them. This pins that the promtool fixture
// that does (deploy/monitoring/rule-tests/status-source-counts_test.yml) runs THESE
// expressions and models its case: a source hosted by the API binary (`massive`, the fiat-FX
// worker) publishing its own stellarindex_source_enabled series. Without that gauge the feed
// is invisible to both counts. The fixture proves both counts rise by one once the series
// exists; this test keeps it honest about which queries and that it still shows the delta.

const statusSourceCountsFixture = "deploy/monitoring/rule-tests/status-source-counts_test.yml"

// statusSourceCountsDoc is the slice of the promtool unit-test schema
// this guard reads. Field names follow promtool's.
type statusSourceCountsDoc struct {
	Tests []struct {
		Name        string `yaml:"name"`
		InputSeries []struct {
			Series string `yaml:"series"`
		} `yaml:"input_series"`
		PromQLExprTest []struct {
			Expr       string `yaml:"expr"`
			ExpSamples []struct {
				Value float64 `yaml:"value"`
			} `yaml:"exp_samples"`
		} `yaml:"promql_expr_test"`
	} `yaml:"tests"`
}

// normalizePromQL collapses whitespace so the YAML block scalar and the
// Go raw string compare on their tokens, which is all Prometheus sees.
func normalizePromQL(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// massiveEnabledSeriesRE matches a fixture series line for the gauge
// the API binary now publishes for massive, in either label order.
var massiveEnabledSeriesRE = regexp.MustCompile(
	`^stellarindex_source_enabled\{(?:[^}]*,)?job="stellarindex_api"(?:,[^}]*)?\}$`)

func TestStatusSourceCountQueries_MatchPromtoolFixture(t *testing.T) {
	path := filepath.Join(repoRootForClaims(t), statusSourceCountsFixture)
	raw, err := os.ReadFile(path) //nolint:gosec // repo-relative, test-only
	if err != nil {
		t.Fatalf("read %s: %v", statusSourceCountsFixture, err)
	}
	var doc statusSourceCountsDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", statusSourceCountsFixture, err)
	}
	if len(doc.Tests) < 2 {
		t.Fatalf("%s has %d cases, want at least the before/after pair", statusSourceCountsFixture, len(doc.Tests))
	}

	wantActive := normalizePromQL(activeSourcesQuery)
	wantTotal := normalizePromQL(totalSourcesQuery)

	// Per case: the expected count for each query, and whether the
	// fixture publishes massive's enabled series from the API job.
	type outcome struct {
		total, active float64
		massiveGauge  bool
	}
	var before, after *outcome
	for _, tc := range doc.Tests {
		o := outcome{total: -1, active: -1}
		for _, s := range tc.InputSeries {
			if massiveEnabledSeriesRE.MatchString(s.Series) && strings.Contains(s.Series, `source="massive"`) {
				o.massiveGauge = true
			}
		}
		for _, e := range tc.PromQLExprTest {
			if len(e.ExpSamples) != 1 {
				t.Fatalf("case %q: expression has %d expected samples, want exactly one scalar count", tc.Name, len(e.ExpSamples))
			}
			switch got := normalizePromQL(e.Expr); got {
			case wantTotal:
				o.total = e.ExpSamples[0].Value
			case wantActive:
				o.active = e.ExpSamples[0].Value
			default:
				t.Errorf("case %q evaluates an expression that is neither totalSourcesQuery nor "+
					"activeSourcesQuery — the fixture has drifted from status.go and no longer "+
					"proves anything about the served counts:\n%s", tc.Name, e.Expr)
			}
		}
		if o.total < 0 || o.active < 0 {
			t.Fatalf("case %q must evaluate BOTH status queries (total=%v active=%v)", tc.Name, o.total, o.active)
		}
		if o.active > o.total {
			t.Errorf("case %q expects active %v > total %v — the numerator is a strict subset of "+
				"the denominator by construction", tc.Name, o.active, o.total)
		}
		if o.massiveGauge {
			after = &o
		} else {
			before = &o
		}
	}

	// Fixture: one host without the API-published gauge, one with; the gauge is worth exactly one on each count, since
	// the feed is on and emitting and so lands in the numerator as well as the denominator.
	if before == nil {
		t.Fatalf("%s has no case without stellarindex_source_enabled{job=\"stellarindex_api\",source=\"massive\"} — "+
			"the pre-fix shape is what the delta is measured against", statusSourceCountsFixture)
	}
	if after == nil {
		t.Fatalf("%s has no case publishing stellarindex_source_enabled{job=\"stellarindex_api\",source=\"massive\"} — "+
			"the fixture no longer models the API-hosted source it exists for", statusSourceCountsFixture)
	}
	if d := after.total - before.total; d != 1 {
		t.Errorf("total_sources moves by %v when the API publishes massive's enabled series, want exactly 1 "+
			"(before=%v after=%v)", d, before.total, after.total)
	}
	if d := after.active - before.active; d != 1 {
		t.Errorf("active_sources moves by %v when the API publishes massive's enabled series, want exactly 1 "+
			"(before=%v after=%v) — an emitting, switched-on source must count on both sides", d, before.active, after.active)
	}
}
