package ingest

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// TestBackfillChunkCoverage_PartialWalkFails: a short but non-empty walk (archive top not yet mirrored) must fail, not refresh CAGGs and exit 0.
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

// TestBackfillChunkCoverage_ZeroWalkStillFails: a zero walk still errors and the message names the bucket.
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

// TestBackfillChunkCoverage_CompleteWalksPass: complete walks, including a resumed chunk owing only ledgers above its cursor, pass.
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

// TestRunBackfillChunk_CoverageCheckPrecedesRefreshAndCompletion pins at the source that the coverage check precedes the CAGG refresh and the completion log.
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

// TestBackfillChunkInterrupted_FailsOnCancel: a SIGINT/SIGTERM mid-chunk must return an error, not nil over an unrefreshed range.
func TestBackfillChunkInterrupted_FailsOnCancel(t *testing.T) {
	t.Parallel()
	chunk := chunkRange{from: 62_800_000, to: 62_900_000}
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		err := backfillChunkInterrupted(cause, chunk, 62_850_000)
		if err == nil {
			t.Fatalf("interrupted chunk (%v) returned nil — backfill exits 0 on a partial range", cause)
		}
		if !errors.Is(err, cause) {
			t.Errorf("error does not wrap %v: %v", cause, err)
		}
		for _, want := range []string{"[62800000,62900000]", "62850000", "-resume"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name %q", err, want)
			}
		}
	}
	if err := backfillChunkInterrupted(nil, chunk, 62_900_000); err != nil {
		t.Errorf("live ctx must not fail the chunk: %v", err)
	}
}

// TestInterruptedCheckpoint_NeverMarksChunkComplete: an interrupt after the last ledger must not mark the chunk done, or -resume skips the CAGG refresh.
func TestInterruptedCheckpoint_NeverMarksChunkComplete(t *testing.T) {
	t.Parallel()
	chunk := chunkRange{from: 100, to: 200}
	cases := []struct{ enqueued, want uint32 }{
		{0, 0},
		{150, 150},
		{199, 199},
		{200, 199},
	}
	for _, tc := range cases {
		if got := interruptedCheckpoint(chunk, tc.enqueued); got != tc.want {
			t.Errorf("interruptedCheckpoint(enqueued=%d) = %d, want %d", tc.enqueued, got, tc.want)
		}
	}
}

// TestRunBackfillChunk_InterruptFailsChunk pins at the source that the interrupt branch returns the chunk error before the CAGG refresh.
func TestRunBackfillChunk_InterruptFailsChunk(t *testing.T) {
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
	if strings.Contains(body, "nolint:nilerr") {
		t.Error("runBackfillChunk still swallows an error as nil (nilerr) — an interrupted chunk exits 0")
	}
	interrupt := strings.Index(body, "ierr := backfillChunkInterrupted(ctx.Err(), chunk, lastFullyEnqueued); ierr != nil")
	ret := strings.Index(body, "return ierr")
	refresh := strings.Index(body, "refreshCAGGsForChunk(")
	complete := strings.Index(body, `logger.Info("chunk complete"`)
	switch {
	case interrupt < 0 || ret < interrupt:
		t.Fatal("runBackfillChunk does not fail an interrupted chunk via backfillChunkInterrupted")
	case refresh < 0 || complete < 0:
		t.Fatal("refresh / completion anchors not found — this test is asserting nothing")
	case interrupt > refresh || interrupt > complete:
		t.Error("interrupt check runs after the CAGG refresh / completion line")
	}
}
