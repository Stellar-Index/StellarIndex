package v1

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

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

// #328: the lean test-net deployments run NO aggregator (ansible
// inventory `run_aggregator: false`) — its absence is a deliberate
// deployment shape, not an outage. The handler nevertheless hardcoded
// {"indexer","aggregator"} as the services to report, so the aggregator
// sat at "unknown" forever, the mixed known/unknown branch of
// rollupOverall pinned `overall` at "degraded", and the public status
// page for testnet.stellarindex.io was red by construction on a
// perfectly healthy deployment.
//
// With the deployment's own service list declared, a healthy indexer
// alone rolls up to "ok" and the aggregator is not reported at all.
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
			freshness: StatusFreshness{ActiveSources: 1, TotalSources: 1},
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

// The opt-out must not become a blindfold: a service the deployment
// DOES declare and whose heartbeat is missing is still partial
// visibility, so `overall` degrades exactly as before. This is the
// fail-closed half of the fix — without it, dropping a service from
// status_services would silently hide a real outage.
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

// #328: the deployment TIER was the string literal "production" at the
// Options construction site, so api.testnet.stellarindex.io answered
// /v1/status with `"deployment":"production"` and the explorer tagged a
// test net PRODUCTION. It is an operator fact, so it comes from config.
func TestStatus_DeploymentTierIsReportedVerbatim(t *testing.T) {
	srv := New(Options{RegionName: "testnet", RegionDeployment: "testnet"})

	st := decodeStatus(t, srv)
	if st.Region.Deployment != "testnet" {
		t.Errorf("Region.Deployment = %q, want testnet", st.Region.Deployment)
	}
}
