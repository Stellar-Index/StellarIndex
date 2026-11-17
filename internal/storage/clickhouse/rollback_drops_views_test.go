package clickhouse

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	rollbackMVRe   = regexp.MustCompile(`(?is)CREATE\s+MATERIALIZED\s+VIEW\s+(?:IF\s+NOT\s+EXISTS\s+)?stellar\.(\w+)\s+TO\s+stellar\.(\w+)`)
	rollbackDropRe = regexp.MustCompile(`(?i)DROP\s+(?:TABLE|VIEW)\s+(?:IF\s+EXISTS\s+)?stellar\.(\w+)`)
	sqlCommentRe   = regexp.MustCompile(`(?m)^\s*--\s?`)
)

// TestRunbookRollbackDropsEveryFeedingViewFirst: a view whose TO target is gone fails every INSERT into its source
// table, so a ROLLBACK that drops a table must first drop every view the same runbook points at it.
func TestRunbookRollbackDropsEveryFeedingViewFirst(t *testing.T) {
	root := lockstepRepoRoot(t)
	files, err := filepath.Glob(filepath.Join(root, "deploy", "clickhouse", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("glob deploy/clickhouse/*.sql: %d files, err %v", len(files), err)
	}
	checked := 0
	for _, path := range files {
		rel, _ := filepath.Rel(root, path)
		src := lockstepReadFile(t, root, rel)
		idx := strings.Index(src, "── ROLLBACK")
		if idx < 0 {
			continue
		}
		feeders := map[string][]string{}
		for _, m := range rollbackMVRe.FindAllStringSubmatch(sqlCommentRe.ReplaceAllString(src, ""), -1) {
			feeders[m[2]] = append(feeders[m[2]], m[1])
		}
		dropped := map[string]bool{}
		for _, m := range rollbackDropRe.FindAllStringSubmatch(src[idx:], -1) {
			for _, mv := range feeders[m[1]] {
				if !dropped[mv] {
					t.Errorf("%s: ROLLBACK drops stellar.%s while view stellar.%s still writes to it; drop the view first", rel, m[1], mv)
				}
			}
			dropped[m[1]] = true
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no ROLLBACK DROP statements parsed; the guard checked nothing")
	}
}

// TestRunbookCutoverStopsTheHealer: a cut-over drops a table's MVs and renames it; a ch-live-catchup heal landing in
// that window writes ledgers below the tip that reach neither table and lie below the gap-close.
func TestRunbookCutoverStopsTheHealer(t *testing.T) {
	root := lockstepRepoRoot(t)
	files, err := filepath.Glob(filepath.Join(root, "deploy", "clickhouse", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("glob deploy/clickhouse/*.sql: %d files, err %v", len(files), err)
	}
	stopRe := regexp.MustCompile(`(?i)stop ch-live-catchup\.timer`)
	cutovers := 0
	for _, path := range files {
		rel, _ := filepath.Rel(root, path)
		src := lockstepReadFile(t, root, rel)
		if !strings.Contains(src, "── Step") || !strings.Contains(src, "RENAME TABLE") {
			continue
		}
		cutovers++
		if !stopRe.MatchString(src) {
			t.Errorf("%s: cut-over runbook never stops ch-live-catchup.timer", rel)
		}
	}
	if cutovers == 0 {
		t.Fatal("no cut-over runbook found; the guard checked nothing")
	}
}
