package archivecompleteness_test

import (
	"os"
	"strings"
	"testing"
)

// TestDocDoesNotClaimUnshippedPrimaryCheck guards against the package
// docs re-asserting that the primary (galexie-archive) scan is
// implemented. Report.Primary is never populated anywhere in this
// package (T232): no code shells out to `galexie detect-gaps`, so the
// doc comments must say so plainly instead of claiming the mode ships
// or carrying a stale "PR B fills it" placeholder.
func TestDocDoesNotClaimUnshippedPrimaryCheck(t *testing.T) {
	staleClaims := []string{
		"Modes (all shipped)",
		"primary is via\n//     shell-out to `galexie detect-gaps`",
		"PR A leaves this nil; PR B fills it",
	}

	for _, file := range []string{"doc.go", "report.go"} {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		content := string(data)
		for _, claim := range staleClaims {
			if strings.Contains(content, claim) {
				t.Errorf("%s still contains stale claim %q: the primary archive check is not implemented (no exec.Command/shell-out in this package)", file, claim)
			}
		}
	}
}
