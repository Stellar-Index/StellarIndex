package ingest

import (
	"os"
	"strings"
	"testing"
)

// backfillRouter opens Postgres before it streams, so its bucket default is
// pinned at the source: it must resolve through opsutil.HistoricReadBucket.
func TestBackfillRouter_DefaultsToArchiveBucket(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("backfill_router.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	start := strings.Index(body, "func backfillRouter(")
	if start < 0 {
		t.Fatal("backfillRouter not found")
	}
	body = body[start:]
	if end := strings.Index(body[1:], "\nfunc "); end >= 0 {
		body = body[:end+1]
	}
	if !strings.Contains(body, "opsutil.HistoricReadBucket(cfg, *bucket)") {
		t.Error("backfillRouter must resolve its bucket via opsutil.HistoricReadBucket (archive, then live)")
	}
	if strings.Contains(body, "cfg.Storage.S3BucketLive") {
		t.Error("backfillRouter still names the TRIMMED live bucket as its default; a historic range walks 0 ledgers")
	}
}
