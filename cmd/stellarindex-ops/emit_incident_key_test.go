package main

import (
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/incidents"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// TestIncidentEventKey_StableAcrossRuns — GH-968: the key a re-run is
// deduplicated on must not carry the emit time (the payload's `at`), or a
// re-run after a partial fan-out re-pages every subscriber. It must still
// separate the SEV-1 from the resolution of the same incident.
func TestIncidentEventKey_StableAcrossRuns(t *testing.T) {
	started := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	resolved := started.Add(90 * time.Minute)
	found := &incidents.Incident{
		Slug:       "2026-09-01-db-outage",
		Severity:   incidents.SeverityMajor,
		Status:     incidents.StatusResolved,
		StartedAt:  started,
		ResolvedAt: &resolved,
	}

	sev1 := incidentEventKey(found, platform.WebhookEventIncidentSEV1)
	if want := "2026-09-01-db-outage@2026-09-01T10:00:00Z"; sev1 != want {
		t.Errorf("sev1 key = %q, want %q", sev1, want)
	}
	res := incidentEventKey(found, platform.WebhookEventIncidentResolved)
	if want := sev1 + "/resolved@2026-09-01T11:30:00Z"; res != want {
		t.Errorf("resolved key = %q, want %q", res, want)
	}
	time.Sleep(time.Millisecond)
	if again := incidentEventKey(found, platform.WebhookEventIncidentSEV1); again != sev1 {
		t.Errorf("key changed between runs: %q then %q", sev1, again)
	}

	later := *found
	later.StartedAt = started.Add(24 * time.Hour)
	if incidentEventKey(&later, platform.WebhookEventIncidentSEV1) == sev1 {
		t.Error("a later incident under the same slug shares the earlier one's key")
	}
}
