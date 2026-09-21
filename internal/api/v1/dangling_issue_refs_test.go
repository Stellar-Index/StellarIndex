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
