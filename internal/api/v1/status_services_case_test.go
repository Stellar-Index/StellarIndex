// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"testing"
	"time"
)

// Regression suite for wave-D RD-05: `api.status_services` was
// validated case-insensitively and consumed case-sensitively.
//
// config/validate.go lower-cases each entry before checking it against
// {indexer, aggregator}. statusServicesOr only TRIMMED. The heartbeat
// map is keyed by Prometheus `job` labels with the `stellarindex-`
// prefix stripped, so its keys are always lower-case.
//
// The result: `status_services = ["Indexer", "Aggregator"]` booted
// without complaint, then reported both services "unknown" on every
// /v1/status request forever. `overall` never left degraded, the
// explorer's status page stayed amber, and an operator debugging it
// found a config value that passed validation and matched the
// documented vocabulary — which is precisely the symptom the list was
// added (#328) to remove.

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

// TestCapitalisedStatusServiceResolvesItsHeartbeat is the end-to-end
// statement of the bug: a capitalised declaration must grade against
// the same lower-cased heartbeat key Prometheus publishes, not sit at
// "unknown" forever.
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
