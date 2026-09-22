package v1

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// danglingMarketsPRRef is the RSWP-094 citation markets.go's source-filter
// doc comment used to carry: "PR #1134" was never a real PR, and the bare
// number now resolves to an unrelated open issue (12-of-27 xdrjson decode
// arms), not the /v1/coins cursor-guard pattern it was cited for. Built
// from parts so this guard's own source doesn't trip the check it performs.
var danglingMarketsPRRef = "PR #" + "1134"

// TestMarketsSourceFilterHasNoDanglingPRReference guards RSWP-094: strip
// dangling citations instead of leaving them pointing a reader at
// unrelated content, and don't invent a replacement PR number we can't
// verify.
func TestMarketsSourceFilterHasNoDanglingPRReference(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed to resolve this test file's path")
	}
	marketsGo := filepath.Join(filepath.Dir(file), "markets.go")
	src, err := os.ReadFile(marketsGo)
	if err != nil {
		t.Fatalf("reading %s: %v", marketsGo, err)
	}
	if bytes.Contains(src, []byte(danglingMarketsPRRef)) {
		t.Errorf("markets.go cites %q, which does not exist and now resolves to an unrelated issue (RSWP-094)", danglingMarketsPRRef)
	}
}
