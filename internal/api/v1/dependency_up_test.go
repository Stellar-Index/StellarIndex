package v1_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// The API has always CHECKED its dependencies on every /v1/readyz round,
// but the outcome existed only as JSON on an HTTP endpoint. Nothing
// scraped it, so nothing could alert on a dependency going away.
//
// That mattered most for ClickHouse (#371 F2): postgres, redis and minio
// each have a Prometheus exporter on r1, and ClickHouse — the raw lake
// the ADR-0033 completeness claim rests on — has none. Its only symptom
// would have been endpoints failing one by one.

type fakeCheck struct {
	name     string
	err      error
	critical bool
}

func (f fakeCheck) Name() string                 { return f.name }
func (f fakeCheck) Ping(_ context.Context) error { return f.err }
func (f fakeCheck) Critical() bool               { return f.critical }

// TestReadyzPublishesDependencyUp is the guard. A readiness check whose
// result never reaches Prometheus is invisible to an operator, which is
// the state this fixes.
func TestReadyzPublishesDependencyUp(t *testing.T) {
	obs.DependencyUp.Reset()

	srv := v1.New(v1.Options{ReadyChecks: []v1.ReadyChecker{
		fakeCheck{name: "postgres", critical: true},
		fakeCheck{name: "clickhouse", err: errors.New("connection refused")},
	}})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/readyz")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("unexpected readyz status %d", resp.StatusCode)
	}

	// Gather the family rather than reading through WithLabelValues.
	// WithLabelValues CREATES a child on read, initialised to 0, so a
	// series that was never published is indistinguishable from one
	// published as 0. A guard written that way passes against code that
	// only ever reports HEALTHY dependencies — the exact silent outage
	// this metric exists to prevent. Verified by mutation: changing the
	// publisher to `if r.OK { Set(1) }` does not fail a ToFloat64-based
	// assertion, and does fail this one.
	got := map[string]float64{}
	families, err := obs.Registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != "stellarindex_dependency_up" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "dependency" {
					got[l.GetValue()] = m.GetGauge().GetValue()
				}
			}
		}
	}

	if v, ok := got["postgres"]; !ok || v != 1 {
		t.Errorf("dependency_up{postgres} = %v (present=%v), want 1", v, ok)
	}
	// The one that matters: a FAILING dependency must be PUBLISHED as 0,
	// not merely absent. `stellarindex_dependency_up == 0` cannot fire on
	// a series that was never emitted.
	v, ok := got["clickhouse"]
	if !ok {
		t.Fatalf("dependency_up{clickhouse} was never published — a failing "+
			"dependency that emits no series cannot be alerted on with `== 0`, "+
			"so the outage stays silent. Published: %v", got)
	}
	if v != 0 {
		t.Errorf("dependency_up{clickhouse} = %v, want 0", v)
	}
}
