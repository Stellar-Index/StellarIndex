package ingest

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// Q209: a SIGINT/SIGTERM mid-chunk checkpointed and returned nil, so
// backfill() saw no chunk error, logged "backfill complete" and exited 0
// over a partially walked, un-refreshed range.
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

// An interrupt that lands after the last ledger drained must not record the
// chunk as done: -resume short-circuits on cursor >= chunk.to and would
// skip the CAGG refresh the interrupt prevented.
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

// runBackfillChunk needs S3 and Postgres, so pin the wiring at the source:
// the interrupt branch returns the chunk error, not nil, and runs before
// the CAGG refresh and the "chunk complete" line.
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
