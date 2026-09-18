package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestClickhouseReadyChecks_ConfiguredButUnreachableStillPublishes: the
// "ClickHouse was already down when the API started" state used to
// publish no `stellarindex_dependency_up{dependency="clickhouse"}`
// series at all, because the checker was appended inside the success
// branch of the boot dial. The alert over that gauge is
// `stellarindex_dependency_up == 0` — deliberately not absent() — so
// the one state the runbook calls "the only signal that it is gone" was
// the state with no signal: every lake-backed endpoint 503'd and
// nothing paged.
func TestClickhouseReadyChecks_ConfiguredButUnreachableStillPublishes(t *testing.T) {
	dialErr := errors.New("dial tcp 127.0.0.1:9000: connect: connection refused")
	checks := clickhouseReadyChecks("localhost:9000", nil, dialErr)
	if len(checks) != 1 {
		t.Fatalf("a configured but unreachable ClickHouse registered %d readiness check(s), want 1 — with none, the gauge is never published and the == 0 alert has nothing to match", len(checks))
	}
	c := checks[0]
	if got := c.Name(); got != "clickhouse" {
		t.Errorf("checker name = %q, want clickhouse (the dependency label the alert selects on)", got)
	}
	if c.Critical() {
		t.Error("the ClickHouse checker is critical — a lake outage would drain the whole backend, where the rest of the API keeps serving from Postgres")
	}
	err := c.Ping(context.Background())
	if err == nil {
		t.Fatal("Ping on a ClickHouse that never dialled returned success — the gauge would publish 1 for a dependency that is gone")
	}
	if !errors.Is(err, dialErr) {
		t.Errorf("Ping error does not wrap the boot dial failure: %v", err)
	}
	if !strings.Contains(err.Error(), "restart") {
		t.Errorf("Ping error does not say a restart is required, which is what recovers the ten un-re-dialled lake seams: %v", err)
	}
}

// A deployment with no lake configured has no such dependency, and must
// publish nothing rather than a 0 — a 0 there pages for a component the
// host does not run.
func TestClickhouseReadyChecks_UnconfiguredPublishesNothing(t *testing.T) {
	if got := clickhouseReadyChecks("", nil, nil); len(got) != 0 {
		t.Fatalf("an unconfigured ClickHouse registered %d readiness check(s), want 0", len(got))
	}
}
