package ingest

import (
	"os"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/config"
)

func TestHistoricReadBucket(t *testing.T) {
	t.Parallel()
	cfg := func(archive, live string) config.Config {
		var c config.Config
		c.Storage.S3BucketArchive, c.Storage.S3BucketLive = archive, live
		return c
	}
	cases := []struct {
		name, archive, live, override, want string
	}{
		{"archive preferred over trimmed live", "galexie-archive", "galexie-live", "", "galexie-archive"},
		{"live when no archive configured", "", "galexie-live", "", "galexie-live"},
		{"explicit override wins", "galexie-archive", "galexie-live", "custom", "custom"},
	}
	for _, tc := range cases {
		got, err := historicReadBucket(cfg(tc.archive, tc.live), tc.override)
		if err != nil || got != tc.want {
			t.Errorf("%s: got (%q, %v), want %q", tc.name, got, err, tc.want)
		}
	}
	if _, err := historicReadBucket(cfg("", ""), ""); err == nil {
		t.Error("no bucket configured and no -bucket: want an error, got nil")
	}
}

// backfillRouter opens Postgres before it streams, so its bucket default is
// pinned at the source: it must resolve through historicReadBucket.
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
	if !strings.Contains(body, "historicReadBucket(cfg, *bucket)") {
		t.Error("backfillRouter must resolve its bucket via historicReadBucket (archive, then live)")
	}
	if strings.Contains(body, "cfg.Storage.S3BucketLive") {
		t.Error("backfillRouter still names the TRIMMED live bucket as its default; a historic range walks 0 ledgers")
	}
}
