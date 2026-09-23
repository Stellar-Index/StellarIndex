package v1

import (
	"os"
	"strings"
	"testing"
)

// TestNoDanglingIssueReferences guards against re-introducing the
// "#1082, #1099-#1105" citation into the clientAborted/timeout doc
// comments (RSWP-083). It was never a live tracking chain — the repo's
// issue/PR count was #803 when the comments were written — and the
// numbers it now collides with belong to unrelated findings, so the
// citation misleads rather than informs. Sites drop the parenthetical
// instead of repointing it, since any fixed number will eventually
// collide with a future issue the same way.
func TestNoDanglingIssueReferences(t *testing.T) {
	const stale = "#1082, #1099-#1105"
	files := []string{
		"clientaborted_test.go",
		"envelope.go",
		"envelope_test.go",
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if strings.Contains(string(b), stale) {
			t.Errorf("%s still cites the dangling range %q; #1105 now resolves to an unrelated issue, not the cold-path timeout guards", f, stale)
		}
	}
}

// TestNoStaleIssueNumber1099 guards against re-introducing "#1099" into
// chart/observations/sources' cold-path timeout-guard comments (RSWP-077).
// #1099 no longer dangles — it now resolves to an unrelated
// supply-canonicalization issue — so citing it here points a reader at
// the wrong thing instead of the 8s-ceiling precedent it once meant.
func TestNoStaleIssueNumber1099(t *testing.T) {
	files := []string{"chart.go", "observations.go", "sources.go"}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if strings.Contains(string(b), "#1099") {
			t.Errorf("%s still cites #1099; that number now resolves to an unrelated supply-canonicalization issue, not this package's timeout-guard precedent", f)
		}
	}
}
