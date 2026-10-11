package v1

import (
	"strings"
	"testing"
)

// The public status page renders "Active sources: N / M" from these two queries; the
// numerator must never exceed its denominator. Two independent faults are pinned.
//
// POPULATION. The numerator must intersect with source_enabled. Always-on supply observers
// (trustlines, sep41_supply, sep41_transfers, sac_balances, liquidity_pools,
// claimable_balances) emit events but carry no `enabled` flag because they are wired into
// the indexer rather than configured, so counting ANY emitting source inflates the numerator.
//
// WINDOW. A short window is far below several enabled sources' publication cadence (rozo,
// phoenix, ecb, band emit a handful of events per day; band's contract publishes no events and
// we observe the relay call instead) and would report them inactive, a false claim about our
// own ingest.
//
// The join makes the ratio coherent BY CONSTRUCTION rather than by the two queries happening
// to agree.

// TestActiveSourcesIsSubsetOfTotal is the load-bearing guard: the
// numerator must be restricted to the same population the denominator
// counts, so N/M can never exceed 1.
func TestActiveSourcesIsSubsetOfTotal(t *testing.T) {
	// The denominator defines the population.
	const population = "stellarindex_source_enabled == 1"
	if !strings.Contains(totalSourcesQuery, population) {
		t.Fatalf("totalSourcesQuery no longer counts %q — this test's premise is "+
			"stale, not merely failing:\n%s", population, totalSourcesQuery)
	}

	// The numerator must intersect with it. Without the join the two
	// count different things and the ratio can exceed 1.
	if !strings.Contains(activeSourcesQuery, "and on (source)") {
		t.Errorf("activeSourcesQuery does not join on (source) with the enabled "+
			"set, so it counts a DIFFERENT population than the denominator and "+
			"the status page can render a numerator larger than its total — it "+
			"read 26/25 on r1. Query:\n%s", activeSourcesQuery)
	}
	if !strings.Contains(activeSourcesQuery, population) {
		t.Errorf("activeSourcesQuery does not restrict to %q. Query:\n%s",
			population, activeSourcesQuery)
	}
}

// TestActiveSourcesWindowToleratesSlowSources — a window shorter than a
// legitimately-enabled source's publication cadence reports it as
// inactive, which misstates our own ingest. The measured floor is band,
// which emits nothing on most days by design.
func TestActiveSourcesWindowToleratesSlowSources(t *testing.T) {
	if strings.Contains(activeSourcesQuery, "[10m]") {
		t.Error("activeSourcesQuery is back to a 10-minute window. rozo emitted 2 " +
			"events in 24h on r1, phoenix 58, ecb 27, band 40 — and band's contract " +
			"publishes no events at all, so we observe its relay CALL instead. A " +
			"10-minute window reports those as inactive when nothing is wrong.")
	}
	if !strings.Contains(activeSourcesQuery, "[7d]") {
		t.Errorf("activeSourcesQuery no longer uses a 7-day window; a source "+
			"appearing inactive should mean something actually stopped. Query:\n%s",
			activeSourcesQuery)
	}
}

// TestActiveSourcesQueriesAreWellFormed catches the shape of a
// copy-paste slip — an unbalanced count() would fail at query time
// against Prometheus, where the failure is a silently absent field
// rather than an error the page shows.
func TestActiveSourcesQueriesAreWellFormed(t *testing.T) {
	for name, q := range map[string]string{
		"activeSourcesQuery": activeSourcesQuery,
		"totalSourcesQuery":  totalSourcesQuery,
	} {
		if !strings.HasPrefix(strings.TrimSpace(q), "count(") {
			t.Errorf("%s does not start with count() — the callers assign its "+
				"scalar result straight to an int field", name)
		}
		if strings.Count(q, "(") != strings.Count(q, ")") {
			t.Errorf("%s has unbalanced parentheses:\n%s", name, q)
		}
	}
}
