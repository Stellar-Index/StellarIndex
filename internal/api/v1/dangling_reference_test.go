package v1_test

import (
	"os"
	"strings"
	"testing"
)

// TestNoDanglingPR1015Reference guards RSWP-066: PR #1015 never existed in
// this repo, and issue #1015 now names an unrelated supply-write-path
// finding, so a comment citing "PR #1015" / "(#1015)" for the chart
// stablecoin-proxy fallback sends a reader to the wrong place instead of a
// clean 404. See internal/api/v1/price.go and ohlc_test.go.
func TestNoDanglingPR1015Reference(t *testing.T) {
	for _, f := range []string{"price.go", "ohlc_test.go", "chart.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		src := string(b)
		if strings.Contains(src, "PR #1015") || strings.Contains(src, "(#1015)") {
			t.Errorf("%s still cites PR #1015 / (#1015); #1015 now resolves to an unrelated open issue", f)
		}
	}
}
