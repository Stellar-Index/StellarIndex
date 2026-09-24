package chops

import (
	"os"
	"runtime"
	"strings"
	"testing"
)

// GH-905: backfill-router was never removed from cmd/stellarindex-ops/main.go's
// command table (still `"backfill-router": ingest.Run`) — it was superseded on
// the lake path by ch-rebuild -contract-calls, not retired. ch_rebuild.go's own
// doc comments and flag help must not claim it is "retired": that tells an
// operator the command no longer exists when `stellarindex-ops backfill-router`
// still runs.
func TestChRebuildDocsDoNotCallBackfillRouterRetired(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed to resolve this test's own file path")
	}
	srcPath := strings.TrimSuffix(thisFile, "_doc_test.go") + ".go"
	src, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatalf("reading %s: %v", srcPath, err)
	}
	text := string(src)

	if strings.Contains(text, "retired backfill-router") {
		t.Errorf("%s still calls backfill-router 'retired'; it remains registered in "+
			"cmd/stellarindex-ops/main.go and should be described as superseded, not retired", srcPath)
	}
	if !strings.Contains(text, "superseded backfill-router") {
		t.Errorf("%s should describe backfill-router as superseded (still registered, "+
			"preferred replacement is ch-rebuild -contract-calls)", srcPath)
	}
}
