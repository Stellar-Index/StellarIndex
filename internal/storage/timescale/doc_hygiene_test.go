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

// The LP-reserve observer shipped as Task #55 PR 4/5 (commit
// ecb28f108); "Task #65" in the same internal Task namespace is
// unrelated work.
func TestLPReserveInsertCitesObserverTask(t *testing.T) {
	text := readPackageSource(t, "classic_supply_observations.go")
	if strings.Contains(text, "Task #65") {
		t.Error("classic_supply_observations.go cites Task #65 for the LP-reserve observer; it shipped as Task #55")
	}
	if !strings.Contains(text, "liquidity_pools, Task #55") {
		t.Error(`classic_supply_observations.go must credit the LP-reserve observer as "liquidity_pools, Task #55"`)
	}
}

// The markets-listing perf fix cited its PR number (#20), which no
// longer resolves to that PR after the history rewrite; the fix commit
// cc4ed08ae is the reference the provenance can be recovered from.
func TestMarketsListingHasNoDanglingIssueReference(t *testing.T) {
	text := readPackageSource(t, "markets.go")
	for _, stale := range []string{"(#20)", "#20 perf"} {
		if strings.Contains(text, stale) {
			t.Errorf("markets.go still cites %q; cite fix commit cc4ed08ae instead", stale)
		}
	}
	if got := strings.Count(text, "cc4ed08ae"); got < 2 {
		t.Errorf("markets.go cites fix commit cc4ed08ae %d time(s); want it in both the root-cause and the 24h-scan notes", got)
	}
}
