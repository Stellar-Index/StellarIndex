package v1

import (
	"os"
	"regexp"
	"testing"
)

// TestHistoryTimeoutComment_NoDanglingIssueCitation pins RSWP-080: the 8s
// ceiling comment above tradesInRangeAfterWithAliases must not cite a bare
// issue number. Bare numbers rot — #1102 once 404'd and now silently
// resolves to an unrelated, already-fixed asset-key collision issue, which
// is worse than a 404 because a reader following it lands on the wrong
// history with no signal anything is off. The comment must instead point at
// a durable, content-addressed anchor (the CHANGELOG entry's own heading
// text) that can be grepped and verified rather than resolved through an
// external, renumberable tracker.
func TestHistoryTimeoutComment_NoDanglingIssueCitation(t *testing.T) {
	src, err := os.ReadFile("history.go")
	if err != nil {
		t.Fatalf("read history.go: %v", err)
	}

	danglingRef := regexp.MustCompile(`#11\d\d`)
	if m := danglingRef.FindString(string(src)); m != "" {
		t.Fatalf("history.go cites a bare numeric issue reference %q; "+
			"these renumber and silently start pointing at unrelated "+
			"issues (RSWP-080) — describe the pattern in prose instead", m)
	}

	changelog, err := os.ReadFile("../../../CHANGELOG.md")
	if err != nil {
		t.Fatalf("read CHANGELOG.md: %v", err)
	}
	anchor := "Cold-path 8-second response ceiling"
	if !regexp.MustCompile(regexp.QuoteMeta(anchor)).Match(changelog) {
		t.Fatalf("CHANGELOG.md no longer contains the %q heading that "+
			"history.go's timeout comment now cites in its place", anchor)
	}
}
