package ingest

import (
	"os"
	"strings"
	"testing"
)

// RLT-266: `backfill` failed open on a PARTIAL walk. It errored only on
// walked == 0, while its two siblings (chops.backfillCoverage,
// censusCoverage) fail any short walk. The scenario is the issue's own:
// -from 62,800,000 -to 62,900,000 against the hourly-mirrored archive,
// whose top ~720 ledgers are not mirrored yet. ledgerstream tolerates
// the miss (it is within 65,536 of the walk's own `to`), the walk ends
// without an error at 99,280 of 100,001, and the chunk used to refresh
// the CAGGs, log "chunk complete" and exit 0.
func TestBackfillChunkCoverage_PartialWalkFails(t *testing.T) {
	t.Parallel()
	chunk := chunkRange{from: 62_800_000, to: 62_900_000}
	err := backfillChunkCoverage(chunk, chunk.from, 99_280, "galexie-archive")
	if err == nil {
		t.Fatal("backfillChunkCoverage(walked=99280, want=100001) returned nil — a 721-ledger " +
			"trade hole would exit 0 and be recorded as a completed chunk (RLT-266)")
	}
	for _, want := range []string{"99280 of 100001", "721 ledgers were NOT walked", `"galexie-archive"`, "[62800000,62900000]", "-resume"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("partial-walk error is missing %q: %v", want, err)
		}
	}
	// One ledger short is still short.
	if backfillChunkCoverage(chunk, chunk.from, 100_000, "galexie-archive") == nil {
		t.Error("a walk one ledger short of the range returned nil")
	}
}

// F-0159's total-miss refusal must survive the rewrite, message shape
// included (it names the bucket, which is the usual culprit).
func TestBackfillChunkCoverage_ZeroWalkStillFails(t *testing.T) {
	t.Parallel()
	err := backfillChunkCoverage(chunkRange{from: 2, to: 1_000_000}, 2, 0, "galexie-live")
	if err == nil {
		t.Fatal("walked=0 over a non-empty range returned nil")
	}
	if !strings.Contains(err.Error(), "walked 0 of 999999") || !strings.Contains(err.Error(), `"galexie-live"`) {
		t.Errorf("total-miss error lost its count or bucket: %v", err)
	}
}

// The guard must not fire on the walks that ARE complete — above all a
// resumed chunk, which only owes the ledgers above its prior cursor.
func TestBackfillChunkCoverage_CompleteWalksPass(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		chunk     chunkRange
		startFrom uint32
		walked    uint64
	}{
		{"full chunk", chunkRange{1_000_000, 1_000_999}, 1_000_000, 1000},
		{"single ledger", chunkRange{5, 5}, 5, 1},
		{"resumed mid-chunk", chunkRange{1_000_000, 1_000_999}, 1_000_500, 500},
		{"resumed at the last ledger", chunkRange{1_000_000, 1_000_999}, 1_000_999, 1},
		{"top of the uint32 range", chunkRange{4_294_967_000, 4_294_967_295}, 4_294_967_000, 296},
	}
	for _, tc := range cases {
		if err := backfillChunkCoverage(tc.chunk, tc.startFrom, tc.walked, "galexie-archive"); err != nil {
			t.Errorf("%s: complete walk refused: %v", tc.name, err)
		}
	}
	// A resumed chunk that comes up short is charged against what THIS
	// run owed, not the whole chunk.
	err := backfillChunkCoverage(chunkRange{1_000_000, 1_000_999}, 1_000_500, 400, "galexie-archive")
	if err == nil || !strings.Contains(err.Error(), "400 of 500") {
		t.Errorf("short resumed walk: want an error charging 400 of 500, got: %v", err)
	}
}

// The guard is only worth anything if it runs BEFORE the chunk is
// refreshed and recorded as done. runBackfillChunk needs S3 and Postgres,
// so pin the ordering at the source: inside that function the coverage
// call precedes both the CAGG refresh and the completing log line.
func TestRunBackfillChunk_CoverageCheckPrecedesRefreshAndCompletion(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile("backfill.go")
	if err != nil {
		t.Fatalf("read backfill.go: %v", err)
	}
	src := string(b)
	start := strings.Index(src, "\nfunc runBackfillChunk(")
	if start < 0 {
		t.Fatal("runBackfillChunk not found — this test is asserting nothing")
	}
	body := src[start+1:]
	if end := strings.Index(body, "\nfunc "); end > 0 {
		body = body[:end]
	}
	coverage := strings.Index(body, "backfillChunkCoverage(chunk, startFrom, walked, opts.bucket)")
	refresh := strings.Index(body, "refreshCAGGsForChunk(")
	complete := strings.Index(body, `logger.Info("chunk complete"`)
	switch {
	case coverage < 0:
		t.Fatal("runBackfillChunk never calls backfillChunkCoverage — a partial walk exits 0 (RLT-266)")
	case refresh < 0 || complete < 0:
		t.Fatal("refresh / completion anchors not found — this test is asserting nothing")
	case coverage > refresh:
		t.Error("coverage check runs AFTER the CAGG refresh — a short chunk is materialised as if complete")
	case coverage > complete:
		t.Error(`coverage check runs AFTER the "chunk complete" log line`)
	}
}
