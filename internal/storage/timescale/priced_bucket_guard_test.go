package timescale

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// A price cagg bucket holding only zero-leg trades has NULL vwap, twap and
// OHLC (migration 0187). Every pair-keyed price read in aggregates.go must
// exclude it in SQL, or the row scan fails and the pair stops serving.
func TestPriceReadersExcludeUnpricedBuckets(t *testing.T) {
	src, err := os.ReadFile("aggregates.go")
	if err != nil {
		t.Fatalf("read aggregates.go: %v", err)
	}
	pairKeyed := regexp.MustCompile(`base_asset = \$[12] AND quote_asset = \$[12]`)
	seen := 0
	for i, line := range strings.Split(string(src), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") || !pairKeyed.MatchString(line) {
			continue
		}
		seen++
		if !strings.Contains(line, " IS NOT NULL") {
			t.Errorf("aggregates.go:%d reads a price cagg by pair without `<price column> IS NOT NULL`:\n%s", i+1, line)
		}
	}
	if seen < 30 {
		t.Fatalf("matched %d pair-keyed read(s), want at least 30: the guard pattern no longer matches the readers", seen)
	}
}
