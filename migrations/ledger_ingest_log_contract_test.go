package migrations

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestLedgerIngestLogNotClaimedPostPersist pins what ledger_ingest_log
// is: the indexer writes its row once ProcessLedger has ENQUEUED the
// ledger's events to the async sink, census-backfill writes it with no
// persistence at all, and projected domains are written later by the
// projector. Every place that tells a reader what the row means — the
// catalog comments an operator reads through `\d+`, 0051's header, the
// README register row and ADR-0033 — must say so rather than calling it
// a post-persist "ledger is done" marker (GH #923).
func TestLedgerIngestLogNotClaimedPostPersist(t *testing.T) {
	tableComment := lastCommentOn(t, "COMMENT ON TABLE ledger_ingest_log IS")
	colComment := lastCommentOn(t, "COMMENT ON COLUMN ledger_ingest_log.persisted_at IS")
	for name, got := range map[string]string{
		"table comment":         tableComment,
		"persisted_at comment":  colComment,
		"0051 up header":        upHeader(t, "0051_ledger_ingest_log.up.sql"),
		"README 0051 row":       extractRow(t, readReadme(t), "0051"),
		"ADR-0033 reality note": adr0033RealityNotes(t),
	} {
		lower := strings.ToLower(got)
		if !strings.Contains(lower, "enqueue") {
			t.Errorf("%s does not say the row is written after ENQUEUE:\n%s", name, got)
		}
		if strings.Contains(name, "ADR") {
			continue // the ADR keeps its original decision text below the note
		}
		for _, bad := range []string{"post-persist", "after its events persist", "done-marker", `"this ledger is done" marker`} {
			if strings.Contains(lower, strings.ToLower(bad)) {
				t.Errorf("%s still claims %q:\n%s", name, bad, got)
			}
		}
	}
}

// lastCommentOn returns the statement text of the highest-numbered up
// migration issuing prefix — the string a migrated database holds.
func lastCommentOn(t *testing.T, prefix string) string {
	t.Helper()
	files, err := filepath.Glob("*.up.sql")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	sort.Strings(files)
	var last string
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		s := string(raw)
		if i := strings.LastIndex(s, prefix); i != -1 {
			end := strings.Index(s[i:], ";")
			if end == -1 {
				t.Fatalf("%s: %q statement has no terminating semicolon", f, prefix)
			}
			last = s[i : i+end]
		}
	}
	if last == "" {
		t.Fatalf("no up migration issues %q", prefix)
	}
	return last
}

// adr0033RealityNotes returns ADR-0033's text above its Context heading,
// where amendments to the original decision are recorded.
func adr0033RealityNotes(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "docs", "adr", "0033-completeness-verification-model.md"))
	if err != nil {
		t.Fatalf("read ADR-0033: %v", err)
	}
	s := string(raw)
	idx := strings.Index(s, "\n## Context")
	if idx == -1 {
		t.Fatal("ADR-0033 has no '## Context' heading — update this test's anchor")
	}
	return s[:idx]
}
