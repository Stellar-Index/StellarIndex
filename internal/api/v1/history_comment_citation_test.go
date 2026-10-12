package v1

import (
	"os"
	"regexp"
	"testing"
)

// TestHistoryTimeoutComment_NoDanglingIssueCitation: the timeout comment above tradesInRangeAfterWithAliases must describe the
// pattern in prose, not cite a bare issue number that can silently resolve to an unrelated issue.
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
}
