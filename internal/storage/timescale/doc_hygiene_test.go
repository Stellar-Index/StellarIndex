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

// Internal task numbers do not resolve outside the private tracker.
func TestLPReserveInsertCitesNoObserverTask(t *testing.T) {
	text := readPackageSource(t, "classic_supply_observations.go")
	for _, stale := range []string{"Task #55", "Task #65"} {
		if strings.Contains(text, stale) {
			t.Errorf("classic_supply_observations.go cites %q for the LP-reserve observer", stale)
		}
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
