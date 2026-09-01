package v1

import (
	"strings"
	"testing"
)

// The public status page renders "Active sources: N / M" from these two
// queries. On 2026-09-01 it read **26 / 25** — a numerator larger than
// its own denominator.
//
// Two independent faults produced that, and both are pinned here.
//
// POPULATION. The numerator's comment already promised an intersection
// with source_enabled, but the query never joined on it — it counted
// ANY source emitting events. Six always-on supply observers
// (trustlines, sep41_supply, sep41_transfers, sac_balances,
// liquidity_pools, claimable_balances) emit events and carry no
// `enabled` flag, because they are wired into the indexer rather than
// configured. They inflated the numerator only. Measured on r1: 26
// emitting, 25 enabled, with exactly those six as the difference.
//
// WINDOW. 10 minutes was far shorter than several enabled sources'
// publication cadence — rozo emitted 2 events in 24h, phoenix 58, ecb
// 27, band 40, and band emits nothing at all on most days by design
// (its contract publishes no events; we observe the relay call
// instead). A 10-minute window reported those as inactive, which is a
// claim about our own ingest that is not true.
//
// The join is what makes the ratio coherent BY CONSTRUCTION rather than
// by the two queries happening to agree.

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
