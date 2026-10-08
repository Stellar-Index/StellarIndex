package main

import (
	"strings"
	"testing"
)

// TestCuratedRWASyncHelpMatchesHandler pins curated-rwa-sync's --help entry
// to what the handler actually does. The entry must not name
// rwa_curated_directory or a per-token USD price — neither is true:
// internal/ops/ingest/curated_rwa_sync.go writes
// curated_rwa_published_series via ReplaceCuratedRWAPublished (a monthly
// market-cap total and a subclass split, not per-token prices), and
// curatedRWAWholeResult refuses the WHOLE run on any malformed row rather
// than skipping and keeping it. ReplaceCuratedRWADirectory has zero
// non-test callers.
func TestCuratedRWASyncHelpMatchesHandler(t *testing.T) {
	i := strings.Index(usageBody, "  curated-rwa-sync ")
	if i < 0 {
		t.Fatal("usageBody has no curated-rwa-sync entry")
	}
	// The entry runs until the next line indented like a synopsis
	// ("  <name>"), i.e. the next subcommand.
	entry := usageBody[i:]
	lines := strings.Split(entry, "\n")
	for n, line := range lines[1:] {
		if strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   ") {
			entry = strings.Join(lines[:n+1], "\n")
			break
		}
	}
	if strings.Contains(entry, "rwa_curated_directory") {
		t.Errorf("curated-rwa-sync help still names rwa_curated_directory, which the handler never writes "+
			"(it writes curated_rwa_published_series):\n%s", entry)
	}
	if !strings.Contains(entry, "curated_rwa_published_series") {
		t.Errorf("curated-rwa-sync help does not name curated_rwa_published_series, the table it actually writes:\n%s", entry)
	}
	if strings.Contains(entry, "per-token") || strings.Contains(entry, "per token") {
		t.Errorf("curated-rwa-sync help still claims a per-token price; the handler reads a monthly "+
			"market-cap total and a subclass split, not per-asset rows:\n%s", entry)
	}
	if strings.Contains(entry, "skipped and counted") {
		t.Errorf("curated-rwa-sync help claims malformed rows are skipped and counted; "+
			"curatedRWAWholeResult refuses the WHOLE run on any malformed row:\n%s", entry)
	}
}
