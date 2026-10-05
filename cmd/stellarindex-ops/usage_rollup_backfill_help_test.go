package main

import (
	"strings"
	"testing"
)

// TestUsageRollupBackfillHelpDocumentsWriteFlag — the --help entry is
// what an operator reads first; its synopsis and example must carry
// -write for the same reason (GH #798).
func TestUsageRollupBackfillHelpDocumentsWriteFlag(t *testing.T) {
	i := strings.Index(usageBody, "  usage-rollup-backfill ")
	if i < 0 {
		t.Fatal("usageBody has no usage-rollup-backfill entry")
	}
	// The entry runs until the next line indented like a synopsis
	// ("  <name>"), i.e. the next subcommand.
	synopsis, entry, _ := strings.Cut(usageBody[i:], "\n")
	for n, line := range strings.Split(entry, "\n") {
		if strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   ") {
			entry = strings.Join(strings.Split(entry, "\n")[:n], "\n")
			break
		}
	}
	if !strings.Contains(synopsis, "[-write]") {
		t.Errorf("usage-rollup-backfill synopsis lacks [-write]: %q", synopsis)
	}
	if !strings.Contains(entry, "-to 2026-07-21 -write") {
		t.Errorf("usage-rollup-backfill --help example does not pass -write:\n%s", entry)
	}
}
