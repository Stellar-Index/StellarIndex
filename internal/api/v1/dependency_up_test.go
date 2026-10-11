package v1_test

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// The /v1/readyz dependency checks must also be exported as a metric, since nothing scrapes the
// JSON endpoint and so nothing could alert on a dependency going away. This matters most for
// ClickHouse: postgres, redis and minio have Prometheus exporters, but ClickHouse (the raw lake
// the ADR-0033 completeness claim rests on) has none.

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

	// Gather the family rather than reading through WithLabelValues, which CREATES a child on
	// read initialised to 0, so a never-published series looks like one published as 0. Such a
	// guard passes against code that only ever reports HEALTHY dependencies, the exact silent
	// outage this metric exists to prevent (a publisher doing `if r.OK { Set(1) }` passes it).
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

// toggleCheck fails while down is set, so a test can move a dependency
// between states without touching the HTTP surface.
type toggleCheck struct{ down *atomic.Bool }

func (c toggleCheck) Name() string   { return "clickhouse" }
func (c toggleCheck) Critical() bool { return false }
func (c toggleCheck) Ping(_ context.Context) error {
	if c.down.Load() {
		return errors.New("connection refused")
	}
	return nil
}

// TestReadinessProbeRefreshesDependencyUpWithoutTraffic: the gauge must track
// a dependency that goes down after the last /v1/readyz call — here there is
// no call at all — or an outage with no probe polling never reads 0.
func TestReadinessProbeRefreshesDependencyUpWithoutTraffic(t *testing.T) {
	obs.DependencyUp.Reset()
	var down atomic.Bool
	srv := v1.New(v1.Options{ReadyChecks: []v1.ReadyChecker{toggleCheck{down: &down}}})

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		srv.StartReadinessProbe(ctx, 10*time.Millisecond)
	}()
	t.Cleanup(func() { cancel(); <-stopped })

	waitDependencyUp(t, "clickhouse", 1)
	down.Store(true)
	waitDependencyUp(t, "clickhouse", 0)
}

func waitDependencyUp(t *testing.T, dep string, want float64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
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
					if l.GetName() == "dependency" && l.GetValue() == dep && m.GetGauge().GetValue() == want {
						return
					}
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("dependency_up{%s} never reached %v without /v1/readyz traffic", dep, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
