package forex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The external-pollers runbook is what on-call follows while massive is dry.
// It must name the fallback the worker actually runs and the log line that
// shows it serving (fetchRates), not a remedy that never reaches the snap.
func TestFXFeedStaleRunbookNamesTheWiredFallback(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "docs", "operations", "runbooks", "external-pollers.md"))
	if err != nil {
		t.Fatalf("read runbook: %v", err)
	}
	doc := string(b)
	name := ECBProvider{}.Name()
	for _, want := range []string{
		"forex.ECBProvider",
		"forex: primary failed — serving from fallback",
		"fallback=" + name,
		"source = '" + name + "'",
		`stellarindex_external_fx_last_quote_unix{source="` + name + `"}`,
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("external-pollers.md does not mention %q; the runbook must point on-call at the wired fallback", want)
		}
	}
}
