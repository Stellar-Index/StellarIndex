package frankfurter

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// danglingBacklog47Ref is the RSWP-024 citation client.go's package doc
// used to carry: "BACKLOG #47" resolves (via `gh pr view 47`) to an
// unrelated merged dependency-bump PR, not the FX-into-external fold it
// was cited for. CHANGELOG.md records the fold under ROADMAP #47, not
// BACKLOG #47. Built from parts so this guard's own source doesn't trip
// the check it performs.
var danglingBacklog47Ref = "BACKLOG #" + "47"

// TestFrankfurterDocHasNoDanglingBacklogReference guards RSWP-024: the
// FX-into-external fold citation in client.go's package doc must point at
// the roadmap item that actually documents the fold, not a stale/wrong
// backlog number that now resolves to unrelated content.
func TestFrankfurterDocHasNoDanglingBacklogReference(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed to resolve this test file's path")
	}
	clientGo := filepath.Join(filepath.Dir(file), "client.go")
	src, err := os.ReadFile(clientGo)
	if err != nil {
		t.Fatalf("reading %s: %v", clientGo, err)
	}
	if bytes.Contains(src, []byte(danglingBacklog47Ref)) {
		t.Errorf("client.go cites %q for the FX-into-external fold, but that resolves to an unrelated merged dependency-bump PR; the fold is documented under ROADMAP #47 (RSWP-024)", danglingBacklog47Ref)
	}
}
