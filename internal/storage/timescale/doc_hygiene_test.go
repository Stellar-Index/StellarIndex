package timescale

import (
	"os"
	"strings"
	"testing"
)

func readPackageSource(t *testing.T, name string) string {
	t.Helper()
	src, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(src)
}

// The LP-reserve observer's internal task references were dropped.
func TestLPReserveInsertCitesObserverTask(t *testing.T) {
	text := readPackageSource(t, "classic_supply_observations.go")
	if strings.Contains(text, "Task #65") {
		t.Error("classic_supply_observations.go cites Task #65 for the LP-reserve observer; it shipped as Task #55")
	}
	if !strings.Contains(text, "liquidity_pools, Task #55") {
		t.Error(`classic_supply_observations.go must credit the LP-reserve observer as "liquidity_pools, Task #55"`)
	}
}

// A bare issue or PR number no longer resolves after the history rewrite; provenance lives in git.
func TestMarketsListingHasNoDanglingIssueReference(t *testing.T) {
	text := readPackageSource(t, "markets.go")
	for _, stale := range []string{"(#20)", "#20 perf"} {
		if strings.Contains(text, stale) {
			t.Errorf("markets.go still cites %q, which no longer resolves", stale)
		}
	}
}
