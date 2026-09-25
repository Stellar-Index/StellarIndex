package customerwebhook

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestRetryWindowMatchesOperatorDocs derives the default retry window from
// backoffCeiling and requires every doc that states it to quote the same
// figure. The budget used to be stated in several places and derived in
// one, and the incident runbook carried a 72 h figure the code never had.
func TestRetryWindowMatchesOperatorDocs(t *testing.T) {
	var lo, hi time.Duration
	// Attempt n's failure schedules retry n; the defaultMaxAttempts-th
	// failure is terminal, so there are defaultMaxAttempts-1 waits.
	for n := 1; n < defaultMaxAttempts; n++ {
		c := backoffCeiling(n)
		hi += c
		lo += c / 2
	}
	want := fmt.Sprintf("~%d–%d h", int(lo.Round(time.Hour).Hours()), int(hi.Round(time.Hour).Hours()))
	if want != "~4–8 h" {
		t.Fatalf("retry window is now %s (%s..%s): update every doc below and this pin together", want, lo, hi)
	}

	stale := regexp.MustCompile(`\b72 ?h\b`)
	root := filepath.Join("..", "..")
	for _, rel := range []string{
		"docs/operations/runbooks/customer-webhook-fanout-failing.md",
		"docs/operations/runbooks/customer-webhook-delivery-failing.md",
		"docs/architecture/platform-spec.md",
		"internal/retentionreaper/reaper.go",
		"internal/customerwebhook/worker.go",
	} {
		body, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		text := string(body)
		if !strings.Contains(text, want) {
			t.Errorf("%s does not state the retry window %q", rel, want)
		}
		if loc := stale.FindStringIndex(text); loc != nil {
			t.Errorf("%s still states a 72 h retry budget: %q", rel, text[max(0, loc[0]-60):loc[1]])
		}
	}
}
