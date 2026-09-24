package usage

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRollupRecoveryDocsMatchConstants pins the alert annotations and
// the runbook to the constants the worker actually recovers with (GH
// #798: both promised a catch-up the code did not perform). Changing
// either constant fails here until the operator-facing text follows.
func TestRollupRecoveryDocsMatchConstants(t *testing.T) {
	want := []string{
		fmt.Sprintf("%d days", retentionDays),
		fmt.Sprintf("%d days per sweep", catchUpDaysPerSweep),
	}
	runbookWant := []string{
		fmt.Sprintf("%d-day", retentionDays),
		fmt.Sprintf("%d days per sweep", catchUpDaysPerSweep),
		fmt.Sprintf("%d per 5-minute sweep", catchUpDaysPerSweep),
	}
	for path, phrases := range map[string][]string{
		"deploy/monitoring/rules/api.yml":                  want,
		"configs/prometheus/rules.r1/api.yml":              want,
		"docs/operations/runbooks/usage-rollup-failing.md": runbookWant,
	} {
		b, err := os.ReadFile(filepath.Join("..", "..", path))
		if err != nil {
			t.Fatal(err)
		}
		text := strings.Join(strings.Fields(string(b)), " ")
		for _, p := range phrases {
			if !strings.Contains(text, p) {
				t.Errorf("%s does not state %q", path, p)
			}
		}
		if strings.Contains(text, "only ever sweeps TODAY + YESTERDAY") {
			t.Errorf("%s still claims the worker only sweeps today + yesterday", path)
		}
	}
}
