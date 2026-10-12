package incident

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/incidents"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// TestFindIncidentForEmit_RefusesWrongStatus: mismatched severity/status combinations are caught before any network I/O.
func TestFindIncidentForEmit_RefusesWrongStatus(t *testing.T) {
	resolved := time.Now().UTC()
	corpus := []incidents.Incident{
		{
			Slug:      "2026-05-12-firing-sev1",
			Title:     "Redis blip",
			Severity:  incidents.SeverityMajor,
			Status:    incidents.StatusInvestigating,
			StartedAt: time.Now().UTC().Add(-10 * time.Minute),
		},
		{
			Slug:       "2026-05-11-old-resolved",
			Title:      "USDC stale",
			Severity:   incidents.SeverityMajor,
			Status:     incidents.StatusResolved,
			StartedAt:  time.Now().UTC().Add(-24 * time.Hour),
			ResolvedAt: &resolved,
		},
		{
			Slug:      "2026-05-09-minor",
			Title:     "Cache miss bump",
			Severity:  incidents.SeverityMinor,
			Status:    incidents.StatusInvestigating,
			StartedAt: time.Now().UTC().Add(-3 * time.Hour),
		},
	}

	cases := []struct {
		name      string
		slug      string
		event     platform.WebhookEventType
		wantSlug  string
		wantErr   bool
		wantInErr string
	}{
		{
			name:     "sev1 on firing sev-1 incident: ok",
			slug:     "2026-05-12-firing-sev1",
			event:    platform.WebhookEventIncidentSEV1,
			wantSlug: "2026-05-12-firing-sev1",
		},
		{
			name:     "resolved on resolved incident: ok",
			slug:     "2026-05-11-old-resolved",
			event:    platform.WebhookEventIncidentResolved,
			wantSlug: "2026-05-11-old-resolved",
		},
		{
			name:      "sev1 on resolved incident: refused",
			slug:      "2026-05-11-old-resolved",
			event:     platform.WebhookEventIncidentSEV1,
			wantErr:   true,
			wantInErr: "already resolved",
		},
		{
			name:      "resolved on investigating incident: refused",
			slug:      "2026-05-12-firing-sev1",
			event:     platform.WebhookEventIncidentResolved,
			wantErr:   true,
			wantInErr: "set frontmatter status=resolved",
		},
		{
			name:      "sev1 on SEV-2 incident: refused",
			slug:      "2026-05-09-minor",
			event:     platform.WebhookEventIncidentSEV1,
			wantErr:   true,
			wantInErr: "only SEV-1 incidents emit incident.sev1",
		},
		{
			name:      "unknown slug: refused",
			slug:      "2024-01-01-never-was",
			event:     platform.WebhookEventIncidentSEV1,
			wantErr:   true,
			wantInErr: "no incident with slug",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := findIncidentForEmit(corpus, tc.slug, tc.event)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error containing %q, got nil", tc.wantInErr)
				}
				if !strings.Contains(err.Error(), tc.wantInErr) {
					t.Errorf("error %q does not contain %q", err, tc.wantInErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got == nil || got.Slug != tc.wantSlug {
				t.Errorf("got slug %q, want %q", got.Slug, tc.wantSlug)
			}
		})
	}
}

// TestIncidentPayloadFields_OmitsAffectedComponentsWhenNil: the webhook body is a hand-built map, so a nil slice would serialize as null despite the struct's omitempty, a shape /v1/incidents never sends.
func TestIncidentPayloadFields_OmitsAffectedComponentsWhenNil(t *testing.T) {
	found := &incidents.Incident{
		Slug:      "2026-05-12-firing-sev1",
		Title:     "Redis blip",
		Severity:  incidents.SeverityMajor,
		Status:    incidents.StatusInvestigating,
		StartedAt: time.Now().UTC(),
		// AffectedComponents intentionally nil.
	}

	fields := incidentPayloadFields(found, platform.WebhookEventIncidentSEV1)

	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if _, present := decoded["affected_components"]; present {
		t.Errorf("affected_components present in payload = %s, want key absent for a nil slice (matches Incident's omitempty contract)", raw)
	}
}

// TestIncidentPayloadFields_IncludesAffectedComponentsWhenSet: a non-empty slice still round-trips.
func TestIncidentPayloadFields_IncludesAffectedComponentsWhenSet(t *testing.T) {
	found := &incidents.Incident{
		Slug:               "2026-05-12-firing-sev1",
		Title:              "Redis blip",
		Severity:           incidents.SeverityMajor,
		Status:             incidents.StatusInvestigating,
		StartedAt:          time.Now().UTC(),
		AffectedComponents: []string{"api", "aggregator"},
	}

	fields := incidentPayloadFields(found, platform.WebhookEventIncidentSEV1)

	got, ok := fields["affected_components"].([]string)
	if !ok || len(got) != 2 {
		t.Fatalf("affected_components = %#v, want [api aggregator]", fields["affected_components"])
	}
}

// TestEmit_RequiresStatedMode: a run stating neither -write nor -dry-run is refused before any config or network I/O.
func TestEmit_RequiresStatedMode(t *testing.T) {
	err := Emit([]string{"-config", "/nonexistent.toml", "-slug", "x", "-event", "sev1"})
	if !errors.Is(err, opsutil.ErrWriteModeUnstated) {
		t.Fatalf("Emit without a mode = %v, want ErrWriteModeUnstated", err)
	}
	err = Emit([]string{"-config", "/nonexistent.toml", "-slug", "x", "-event", "sev1", "-dry-run"})
	if err == nil || errors.Is(err, opsutil.ErrWriteModeUnstated) {
		t.Fatalf("Emit -dry-run = %v, want it past the mode check to the config load", err)
	}
}

// TestIncidentEventKey_StableAcrossRuns: the dedup key must not carry the emit time (a re-run would re-page everyone) yet must still separate the SEV-1 from its resolution.
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
