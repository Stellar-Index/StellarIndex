package chops

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// recordingPublisher captures what publishSourceVerdict hands the store.
// It stands in for the store only to observe the ARGUMENTS — the
// applied/cleared behaviour itself is proven on real TimescaleDB in
// test/integration/completeness_verdict_publish_test.go.
type recordingPublisher struct {
	clear *timescale.DirtyWindowClear
	calls int
	out   timescale.VerdictPublication
}

func (r *recordingPublisher) PublishCompletenessVerdict(_ context.Context, _ timescale.CompletenessSnapshot, c *timescale.DirtyWindowClear) (timescale.VerdictPublication, error) {
	r.calls++
	r.clear = c
	return r.out, nil
}

// F072: the clear travels INTO the verdict's transaction — identified by
// the exact row this run read — only when the run earned it; an unearned
// run must hand the store no clear at all.
func TestPublishSourceVerdict_ClearOnlyWhenEarned(t *testing.T) {
	stamp := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	win := timescale.ProjectionDirtyWindow{Source: "cctp", From: 62_000_000, To: 62_500_000, UpdatedAt: stamp}

	earned := &recordingPublisher{}
	if _, err := publishSourceVerdict(context.Background(), earned, timescale.CompletenessSnapshot{Source: "cctp"}, win, true); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if earned.calls != 1 || earned.clear == nil {
		t.Fatalf("earned run: calls=%d clear=%v, want one call carrying a clear", earned.calls, earned.clear)
	}
	if *earned.clear != (timescale.DirtyWindowClear{From: 62_000_000, To: 62_500_000, UpdatedAt: stamp}) {
		t.Errorf("clear = %+v, want the exact window this run read (bounds AND updated_at)", *earned.clear)
	}

	unearned := &recordingPublisher{}
	if _, err := publishSourceVerdict(context.Background(), unearned, timescale.CompletenessSnapshot{Source: "cctp"}, win, false); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if unearned.calls != 1 || unearned.clear != nil {
		t.Fatalf("unearned run: calls=%d clear=%+v, want one call with a nil clear", unearned.calls, unearned.clear)
	}
}

// The operator-facing line must not read like a stored verdict when the
// never-regress guard rejected it.
func TestVerdictNotStoredNote(t *testing.T) {
	if got := verdictNotStoredNote(timescale.VerdictPublication{Applied: true, WindowCleared: true}, 63_600_000, true); got != "" {
		t.Errorf("applied verdict: note = %q, want empty", got)
	}
	got := verdictNotStoredNote(timescale.VerdictPublication{}, 63_000_000, true)
	for _, want := range []string{"VERDICT NOT STORED", "63000000", "stays PENDING"} {
		if !strings.Contains(got, want) {
			t.Errorf("rejected verdict with a pending window: note %q lacks %q", got, want)
		}
	}
	if got := verdictNotStoredNote(timescale.VerdictPublication{}, 63_000_000, false); strings.Contains(got, "PENDING") || !strings.Contains(got, "VERDICT NOT STORED") {
		t.Errorf("rejected verdict, no window: note = %q", got)
	}
}
