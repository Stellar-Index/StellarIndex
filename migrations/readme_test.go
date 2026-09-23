package migrations

import (
	"os"
	"strings"
	"testing"
)

// TestReadmeTWAPColumnNotedAsLive guards rule 8 against re-claiming that
// prices_1m's twap column (migration 0002) "is read by nothing": migration
// 0081/0126's twap_1h/twap_1d CAGGs are avg(prices_1m.twap) and ARE read,
// via internal/storage/timescale/aggregates.go::combineDirTWAP (T338).
func TestReadmeTWAPColumnNotedAsLive(t *testing.T) {
	raw := readReadme(t)
	rule8 := extractRule8(t, raw)

	if strings.Contains(rule8, "read by nothing") {
		t.Error(`README.md rule 8 still claims the prices_1m twap column "read by nothing" — it feeds twap_1h/twap_1d, read via combineDirTWAP`)
	}
	if !strings.Contains(rule8, "combineDirTWAP") {
		t.Error("README.md rule 8 should point at combineDirTWAP as the reader of prices_1m.twap via twap_1h/twap_1d")
	}
}

// TestReadmeStripeDeadLetterClaimNotesTombstone guards the 0118/0121 rows
// against describing the stripe_event_log dead-letter/claim columns in
// present tense with no note that migration 0152 dropped the columns and
// deleted their Go writers (T409).
func TestReadmeStripeDeadLetterClaimNotesTombstone(t *testing.T) {
	raw := readReadme(t)

	for _, id := range []string{"0118", "0121"} {
		row := extractRow(t, raw, id)
		if !strings.Contains(row, "Tombstoned by 0152") {
			t.Errorf("%s's row doesn't note that migration 0152 tombstoned its columns/writers", id)
		}
	}
}

func extractRow(t *testing.T, raw, id string) string {
	t.Helper()
	marker := "| " + id + " |"
	idx := strings.Index(raw, marker)
	if idx == -1 {
		t.Fatalf("README.md table row for migration %s not found", id)
	}
	end := strings.Index(raw[idx:], "\n")
	if end == -1 {
		end = len(raw) - idx
	}
	return raw[idx : idx+end]
}

func readReadme(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	return string(raw)
}

func extractRule8(t *testing.T, raw string) string {
	t.Helper()
	start := strings.Index(raw, "8. **Ratio aggregates")
	if start == -1 {
		t.Fatal("README.md rule 8 (Ratio aggregates) not found — update this test's anchor")
	}
	end := strings.Index(raw[start:], "9. **Every up-migration")
	if end == -1 {
		t.Fatal("README.md rule 9 (Every up-migration) not found — update this test's anchor")
	}
	return raw[start : start+end]
}
