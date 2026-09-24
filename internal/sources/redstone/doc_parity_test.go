package redstone

import (
	"os"
	"regexp"
	"strconv"
	"testing"
)

// TestFeedRegistry_CountMatchesProtocolPages extends the doc-comment pin
// to the public protocol pages, which stated three different wrong counts
// after feeds were added.
func TestFeedRegistry_CountMatchesProtocolPages(t *testing.T) {
	pages := map[string][]*regexp.Regexp{
		"../../../docs/protocols/README.md": {
			regexp.MustCompile(`Adapter contract \+ (\d+)-feed registry`),
		},
		"../../../docs/protocols/redstone.md": {
			regexp.MustCompile(`Adapter contract and the (\d+)-feed`),
			regexp.MustCompile(`registry of the (\d+) mainnet`),
			regexp.MustCompile(`holds all (\d+) mainnet feeds`),
		},
	}
	for path, res := range pages {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, re := range res {
			m := re.FindStringSubmatch(string(body))
			if m == nil {
				t.Errorf("%s: no match for %q — the sentence moved; update this test with it", path, re)
				continue
			}
			if n, _ := strconv.Atoi(m[1]); n != len(feedRegistry) {
				t.Errorf("%s: %q says %d feeds, feedRegistry has %d", path, m[0], n, len(feedRegistry))
			}
		}
	}
}
