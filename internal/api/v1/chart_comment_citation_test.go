package v1

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestChartTimeoutComment_NoDanglingIssueCitation pins RSWP-078: the 8s
// chart-query ceiling comment must not cite a bare issue number. #1100 was
// never a live tracking reference (the repo's issue/PR count was #803 when
// the comment was written) and the tracker has since grown past it, so a
// reader following the citation lands on an unrelated, already-fixed issue
// with no signal anything is off. The comment must instead point at a
// durable, content-addressed anchor (the CHANGELOG entry's own heading
// text) that can be grepped and verified rather than resolved through an
// external, renumberable tracker.
func TestChartTimeoutComment_NoDanglingIssueCitation(t *testing.T) {
	src, err := os.ReadFile("chart.go")
	if err != nil {
		t.Fatalf("read chart.go: %v", err)
	}

	const stale = "Same pattern as #1082 / #1099 / #1100 / #1101"
	if strings.Contains(string(src), stale) {
		t.Fatalf("chart.go still cites the dangling range %q; "+
			"these renumber and silently start pointing at unrelated "+
			"issues (RSWP-078) — describe the pattern in prose instead", stale)
	}

	changelog, err := os.ReadFile("../../../CHANGELOG.md")
	if err != nil {
		t.Fatalf("read CHANGELOG.md: %v", err)
	}
	anchor := "Cold-path 8-second response ceiling"
	if !regexp.MustCompile(regexp.QuoteMeta(anchor)).Match(changelog) {
		t.Fatalf("CHANGELOG.md no longer contains the %q heading that "+
			"chart.go's timeout comment now cites in its place", anchor)
	}
}
