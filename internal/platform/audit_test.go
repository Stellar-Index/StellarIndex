package platform_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func readAuditGo(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	src, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "audit.go"))
	if err != nil {
		t.Fatalf("read audit.go: %v", err)
	}
	return string(src)
}

// TestAuditStoreDocDoesNotClaimDashboardReadsViaList (T158): the
// AuditStore interface doc must not claim "the dashboard reads via
// List" — grep across the tree finds zero non-test callers of List,
// and admin_keys.go's AuditSink (the only thing wired into API
// handlers) exposes just Append.
func TestAuditStoreDocDoesNotClaimDashboardReadsViaList(t *testing.T) {
	text := readAuditGo(t)
	if strings.Contains(text, "the dashboard reads via List") {
		t.Error(`audit.go doc comment still claims "the dashboard reads via List"; no dashboard or API route calls List`)
	}
	if !strings.Contains(text, "List is not wired into any dashboard or API route today") {
		t.Error("audit.go's AuditStore doc should say plainly that List isn't wired into any dashboard/API route")
	}
	for _, invented := range []string{"staff/ops tooling", "staff console"} {
		if strings.Contains(text, invented) {
			t.Errorf("audit.go names %q as a List reader; no such tool calls List", invented)
		}
	}
}

// TestAuditEntryDocDoesNotClaimOfflineArchiver (T378): the
// AuditEntry doc must not claim rows are deleted "by the offline
// retention archiver" — no archiver exists anywhere in the tree (no
// DELETE/purge statement touches audit_log; no archiver code in
// migrations/, scripts/, configs/, internal/, cmd/ or deploy/).
func TestAuditEntryDocDoesNotClaimOfflineArchiver(t *testing.T) {
	text := readAuditGo(t)
	if strings.Contains(text, "except by the offline retention archiver") {
		t.Error(`audit.go doc comment still claims rows are deleted "except by the offline retention archiver"; no such archiver exists anywhere in the tree`)
	}
	if !strings.Contains(text, "No retention/archival job exists") {
		t.Error("audit.go's AuditEntry doc comment should state plainly that no retention/archival job exists yet")
	}
}
