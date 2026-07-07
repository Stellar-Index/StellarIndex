package timescale

import (
	"strings"
	"testing"
)

// TestCountRecentEventsBySource_readsRollup asserts the read path is a
// keyed-on-PK lookup against protocol_events_24h — NOT the inline
// multi-table census that the 2026-07-06 latency fix (#43) moved to the
// aggregator worker. If this regresses (someone re-inlines the census)
// the /v1/protocols cold latency comes back.
func TestCountRecentEventsBySource_readsRollup(t *testing.T) {
	if !strings.Contains(readProtocolEventsRollup, "FROM protocol_events_24h") {
		t.Errorf("reader must select FROM protocol_events_24h, got: %q", readProtocolEventsRollup)
	}
	// The reader must not re-run the census (no UNION / count(*) /
	// interval scan on the request path).
	for _, banned := range []string{"UNION ALL", "count(*)", "interval '24 hours'"} {
		if strings.Contains(readProtocolEventsRollup, banned) {
			t.Errorf("reader query must not contain %q (that is the worker's job): %q", banned, readProtocolEventsRollup)
		}
	}
}

// TestRefreshProtocolEventsUpsert_shape asserts the writer folds the
// census into one row per source and upserts idempotently.
func TestRefreshProtocolEventsUpsert_shape(t *testing.T) {
	for _, want := range []string{
		"INSERT INTO protocol_events_24h",
		"SUM(n)::bigint",       // multi-leg-per-source fold (blend 4×, phoenix 3×, …)
		"GROUP BY source",      // one row per logical source
		"ON CONFLICT (source)", // idempotent replace
	} {
		if !strings.Contains(refreshProtocolEventsUpsert, want) {
			t.Errorf("upsert query missing %q:\n%s", want, refreshProtocolEventsUpsert)
		}
	}
	// The upsert must embed the actual census (so it stays in lockstep
	// with countRecentEventsQuery — no second copy to drift).
	if !strings.Contains(refreshProtocolEventsUpsert, "FROM trades") ||
		!strings.Contains(refreshProtocolEventsUpsert, "FROM oracle_updates") {
		t.Errorf("upsert must embed countRecentEventsQuery (trades + oracle_updates legs):\n%s", refreshProtocolEventsUpsert)
	}
}

// TestRefreshProtocolEventsPrune_sargable asserts the prune deletes on a
// bare computed_at comparison (index-friendly, no function on the
// column) so the stale-source sweep stays cheap.
func TestRefreshProtocolEventsPrune_sargable(t *testing.T) {
	if !strings.Contains(refreshProtocolEventsPrune, "computed_at < now()") {
		t.Errorf("prune must compare computed_at < now() directly, got: %q", refreshProtocolEventsPrune)
	}
}
