package bitstamp

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// danglingDustFilterRefs are the RSWP-049 citations events.go's ErrDustTrade
// doc comment and parse_test.go's dust-trade test used to carry: "#814" and
// "#1234" were never this dust filter's origin (they resolve to unrelated
// review-tracking issues after the 2026-09-10 history rewrite reused those
// numbers), so the citations pointed a reader at the wrong thing instead of
// nothing. Built from parts so this guard's own source doesn't trip the
// check it performs.
var danglingDustFilterRefs = []string{"#" + "814", "#" + "1234"}

// TestBitstampDustFilterHasNoDanglingIssueReference guards RSWP-049: strip
// dangling citations instead of leaving them pointing a reader at unrelated
// content, and don't invent a replacement issue number we can't verify.
func TestBitstampDustFilterHasNoDanglingIssueReference(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed to resolve this test file's path")
	}
	dir := filepath.Dir(file)
	for _, name := range []string{"events.go", "parse_test.go"} {
		path := filepath.Join(dir, name)
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		for _, ref := range danglingDustFilterRefs {
			if bytes.Contains(src, []byte(ref)) {
				t.Errorf("%s cites %q, which does not resolve to the dust filter's origin and now points at an unrelated issue (RSWP-049)", name, ref)
			}
		}
	}
}
