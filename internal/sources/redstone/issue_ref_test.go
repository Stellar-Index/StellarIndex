package redstone

import (
	"os"
	"regexp"
	"testing"
)

// TestNoMisPointedIssue53Reference stops the EUROC USD-hardcode fix from
// being re-attributed to GitHub issue #53, which resolves to an unrelated
// "CI health: main has been red" ticket, not this decoder change (RSWP-026).
// The fix landed at commit ecc289c6; comments cite that SHA instead.
func TestNoMisPointedIssue53Reference(t *testing.T) {
	re := regexp.MustCompile(`#53\b`)
	for _, f := range []string{"feeds.go", "decode.go", "decode_test.go"} {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if re.Match(raw) {
			t.Errorf("%s still references '#53' — that issue number resolves to an unrelated closed ticket; cite commit ecc289c6 instead", f)
		}
	}
}
