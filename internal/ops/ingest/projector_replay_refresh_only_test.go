package ingest

import (
	"context"
	"strings"
	"testing"
	"time"
)

// CA2-A19-correct-8: on a -wait-timeout, the recovery advice must not be
// "re-run with the same -from" — a re-run recomputes the rewind target
// from the now partially-advanced cursor and rewinds+re-walks the whole
// range again, never refreshing the gap between the first run's partial
// progress and its original pre-rewind cursor. The advice must instead
// point at the refresh-only recovery path, which refreshes CAGGs over an
// explicit range without touching the cursor.
func TestAwaitProjectorCursor_TimeoutAdvisesRefreshOnly_NotPlainRerun(t *testing.T) {
	f := &fakeProjectorCursor{ledgers: []uint32{62_900_000}}
	err := awaitProjectorCursor(context.Background(), discardLogger(), f, "cctp", 63_500_000, 10*time.Millisecond, time.Millisecond)
	if err == nil {
		t.Fatal("a projector that never re-walked the range returned success")
	}
	msg := err.Error()
	if !strings.Contains(msg, "-refresh-only") {
		t.Errorf("timeout error does not point at the refresh-only recovery path: %v", err)
	}
	if strings.Contains(msg, "re-run this command with the same -from (the rewind is already durable and idempotent)") {
		t.Errorf("timeout error still gives the plain re-run advice that rewinds and re-walks the whole range again, leaving the already-progressed prefix unrefreshed: %v", err)
	}
}

// -refresh-only requires an explicit upper bound and refuses a
// range that doesn't make sense, before touching config or the store.
func TestProjectorReplay_RefreshOnlyRequiresRefreshTo(t *testing.T) {
	t.Parallel()
	err := projectorReplay(replayArgs(t, "cctp", "-refresh-only"))
	if err == nil || !strings.Contains(err.Error(), "-refresh-to") {
		t.Fatalf("missing -refresh-to must be refused, got: %v", err)
	}

	err = projectorReplay(replayArgs(t, "cctp", "-refresh-only", "-refresh-to", "1"))
	if err == nil || !strings.Contains(err.Error(), "-refresh-to") {
		t.Fatalf("-refresh-to below -from must be refused, got: %v", err)
	}
}

// -refresh-only -dry-run never rewinds the cursor and never loads the
// config/store — it prints the range it would refresh and returns.
func TestProjectorReplay_RefreshOnlyDryRun(t *testing.T) {
	t.Parallel()
	var err error
	out := captureStdout(t, func() {
		err = projectorReplay(replayArgs(t, "cctp", "-refresh-only", "-refresh-to", "52728400", "-dry-run"))
	})
	if err != nil {
		t.Fatalf("dry-run refresh-only returned error: %v", err)
	}
	if !strings.Contains(out, "would refresh the price CAGGs over ledgers [52728375,52728400]") {
		t.Errorf("dry-run output missing the planned range: %q", out)
	}
	if strings.Contains(out, "would RecordProjectionDirtyWindow") || strings.Contains(out, "would then UpsertCursor") {
		t.Errorf("refresh-only dry-run must not go through the rewind dry-run path: %q", out)
	}
}
