package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/incidents"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// TestFindIncidentForEmit_RefusesWrongStatus — operator-finger-
// trouble combinations (sev1 on resolved, resolved on
// investigating, sev1 on non-SEV-1) must be caught BEFORE any
// network I/O so the operator can re-check inputs.
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

// TestIncidentPayloadFields_OmitsAffectedComponentsWhenNil pins
// RLT-211: incidents.Incident tags affected_components `omitempty`,
// so an incident with no components published via /v1/incidents
// simply lacks the key. incidentPayloadFields hand-builds a
// map[string]any for the webhook body, where a struct's json tag has
// no bearing on map-value marshaling — a nil slice assigned into the
// map serializes as `"affected_components":null` regardless of the
// tag, giving webhook subscribers a shape /v1/incidents never sends.
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

// TestIncidentPayloadFields_IncludesAffectedComponentsWhenSet is the
// companion positive case: a non-empty slice must still round-trip.
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
